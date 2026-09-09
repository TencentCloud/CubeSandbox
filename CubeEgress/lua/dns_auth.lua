-- CubeSandbox cube-egress - bind L7 domain identity to original dst IP.
--
-- Host/SNI policy matches prove only what name the sandbox presented to the
-- proxy. For transparent proxying we must also prove that the original
-- destination IP belongs to that name according to proxy-side DNS, otherwise a
-- sandbox can forge Host/SNI while connecting to an unrelated IP.

local _M = {}

local RESOLV_CONF = "/etc/resolv.conf"
local CACHE_TTL_MIN = 5
local CACHE_TTL_MAX = 300

local test_resolver

local function lower(s)
    if type(s) ~= "string" then return nil end
    return string.lower(s)
end

local function trim(s)
    if type(s) ~= "string" then return nil end
    return (string.gsub(s, "^%s*(.-)%s*$", "%1"))
end

local IPV4_PATTERN = "^([0-9]+)%.([0-9]+)%.([0-9]+)%.([0-9]+)$"

local function parse_ipv4(s)
    if type(s) ~= "string" then return nil end
    local a, b, c, d = string.match(s, IPV4_PATTERN)
    if not a then return nil end
    local out = {}
    for _, octet in ipairs({a, b, c, d}) do
        if #octet > 1 and string.sub(octet, 1, 1) == "0" then return nil end
        local n = tonumber(octet)
        if not n or n < 0 or n > 255 then return nil end
        out[#out + 1] = tostring(n)
    end
    return table.concat(out, ".")
end

local function normalize_domain(name)
    name = trim(name)
    if not name or name == "" then return nil end
    return string.gsub(lower(name), "%.$", "")
end

local function normalize_resolver_address(addr)
    addr = trim(addr)
    if not addr or addr == "" then return nil end
    local ip, port = string.match(addr, "^([^:]+):([0-9]+)$")
    if ip then
        ip = parse_ipv4(ip)
        port = tonumber(port)
        if ip and port and port > 0 and port <= 65535 then return {ip, port} end
        return nil
    end
    addr = parse_ipv4(addr)
    if not addr then return nil end
    return addr
end

local function resolver_key(ns)
    if type(ns) == "table" then return tostring(ns[1]) .. ":" .. tostring(ns[2]) end
    return tostring(ns)
end

local function append_nameserver(out, seen, ns)
    local normalized = normalize_resolver_address(ns)
    if not normalized then return end
    local key = resolver_key(normalized)
    if seen[key] then return end
    seen[key] = true
    out[#out + 1] = normalized
end

local function nameservers_from_env()
    local raw = os.getenv("CUBE_EGRESS_DNS_RESOLVER_ADDRS")
    if not raw or raw == "" then return nil end
    local out, seen = {}, {}
    for token in string.gmatch(raw, "[^,%s]+") do
        append_nameserver(out, seen, token)
    end
    if #out == 0 then return nil end
    return out
end

local function nameservers_from_resolv_conf(path)
    local f = io.open(path or RESOLV_CONF, "r")
    if not f then return nil end
    local out, seen = {}, {}
    for line in f:lines() do
        local ns = string.match(line, "^%s*nameserver%s+([^%s#;]+)")
        if ns then append_nameserver(out, seen, ns) end
    end
    f:close()
    if #out == 0 then return nil end
    return out
end

local function cache()
    return ngx and ngx.shared and ngx.shared.dns_auth_cache or nil
end

local function clamp_ttl(ttl)
    ttl = tonumber(ttl) or CACHE_TTL_MIN
    if ttl < CACHE_TTL_MIN then return CACHE_TTL_MIN end
    if ttl > CACHE_TTL_MAX then return CACHE_TTL_MAX end
    return math.floor(ttl)
end

local function resolve_ipv4s_uncached(name)
    if test_resolver then return test_resolver(name) end

    local ok_lib, resolver_lib = pcall(require, "resty.dns.resolver")
    if not ok_lib or not resolver_lib then
        return nil, "resolver_module_unavailable:" .. tostring(resolver_lib)
    end
    local nameservers = nameservers_from_env() or nameservers_from_resolv_conf()
    if not nameservers then return nil, "no_proxy_dns_resolver" end
    local resolver, err = resolver_lib:new({
        nameservers = nameservers,
        retrans = 2,
        timeout = 2000,
    })
    if not resolver then return nil, "resolver_init_failed:" .. tostring(err) end

    local qname = name
    local out, seen = {}, {}
    local min_ttl = CACHE_TTL_MAX
    for _ = 1, 5 do
        local answers, qerr = resolver:query(qname, {qtype = resolver.TYPE_A})
        if not answers then return nil, "dns_query_failed:" .. tostring(qerr) end
        if answers.errcode then return nil, "dns_rcode_" .. tostring(answers.errcode) end

        local cname
        for _, ans in ipairs(answers) do
            if ans.address then
                local ip = parse_ipv4(ans.address)
                if ip and not seen[ip] then
                    seen[ip] = true
                    out[#out + 1] = ip
                end
                if ans.ttl then min_ttl = math.min(min_ttl, clamp_ttl(ans.ttl)) end
            elseif ans.cname and not cname then
                cname = normalize_domain(ans.cname)
                if ans.ttl then min_ttl = math.min(min_ttl, clamp_ttl(ans.ttl)) end
            end
        end
        if #out > 0 or not cname or cname == qname then break end
        qname = cname
    end
    return out, nil, clamp_ttl(min_ttl)
end

local function resolve_ipv4s(name)
    local c = cache()
    local list_key = "name:" .. name
    if c then
        local cached = c:get(list_key)
        if cached and cached ~= "" then
            local out = {}
            for ip in string.gmatch(cached, "[^,]+") do
                out[#out + 1] = ip
            end
            return out
        end
    end

    local ips, err, ttl = resolve_ipv4s_uncached(name)
    if not ips then return nil, err end
    if c and #ips > 0 then
        c:set(list_key, table.concat(ips, ","), ttl or CACHE_TTL_MIN)
    end
    return ips
end

local function list_contains(list, value)
    for _, item in ipairs(list or {}) do
        if item == value then return true end
    end
    return false
end

local function verify_name(name, dst_ip)
    dst_ip = parse_ipv4(dst_ip)
    if not dst_ip then return false, "g5_missing_original_dst", {name = name} end

    local literal = parse_ipv4(name)
    if literal then
        if literal == dst_ip then return true, nil, {name = name, ips = {literal}} end
        return false, "g5_ip_literal_mismatch", {
            name = name,
            dst_ip = dst_ip,
            resolved_ips = {literal},
        }
    end

    local domain = normalize_domain(name)
    if not domain then return false, "g5_missing_domain_identity", {name = name} end
    local ips, err = resolve_ipv4s(domain)
    if not ips then
        return false, "g5_dns_resolve_failed", {
            name = domain,
            dst_ip = dst_ip,
            resolver_error = err,
        }
    end
    if not list_contains(ips, dst_ip) then
        return false, "g5_dst_ip_not_in_dns", {
            name = domain,
            dst_ip = dst_ip,
            resolved_ips = ips,
        }
    end
    return true, nil, {name = domain, dst_ip = dst_ip, resolved_ips = ips}
end

function _M.verify_rule(match, ctx)
    if type(match) ~= "table" then return true end
    local names = {}
    if match.host ~= nil then
        if not ctx.host or ctx.host == "" then
            return false, "g5_missing_host_for_dns_auth", {field = "host"}
        end
        names[#names + 1] = {field = "host", value = ctx.host}
    end
    if match.sni ~= nil then
        if not ctx.sni or ctx.sni == "" then
            return false, "g5_missing_sni_for_dns_auth", {field = "sni"}
        end
        names[#names + 1] = {field = "sni", value = ctx.sni}
    end
    if #names == 0 then return true end

    local checked, seen = {}, {}
    for _, entry in ipairs(names) do
        local key = normalize_domain(entry.value) or entry.value
        if not seen[key] then
            seen[key] = true
            local ok, reason, detail = verify_name(entry.value, ctx.dst_ip)
            detail = detail or {}
            detail.field = entry.field
            checked[#checked + 1] = detail
            if not ok then return false, reason, {checks = checked} end
        end
    end
    return true, nil, {checks = checked}
end

function _M._set_test_resolver(fn)
    test_resolver = fn
end

function _M._parse_ipv4(s)
    return parse_ipv4(s)
end

function _M._nameservers_from_resolv_conf(path)
    return nameservers_from_resolv_conf(path)
end

return _M
