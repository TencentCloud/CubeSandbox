-- CubeSandbox cube-egress - bind L7 domain identity to original dst IP.
--
-- Host/SNI policy matches prove only what name the sandbox presented to the
-- proxy. For transparent proxying we must also prove that the original
-- destination IP belongs to that name according to proxy-side DNS, otherwise a
-- sandbox can forge Host/SNI while connecting to an unrelated IP.

local _M = {}

local RESOLV_CONF = "/etc/resolv.conf"
local CACHE_TTL_MAX = 300
local MAX_CNAME_HOPS = 5
local LOOKUP_TIMEOUT = 5
local MAX_INFLIGHT = 16
local MAX_WAITERS = 256
local configured_nameservers, configuration_error
local cache_prefix
local inflight, active_lookups = {}, 0

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
    if type(name) ~= "string" then return nil end
    name = string.gsub(lower(name), "%.$", "")
    if #name == 0 or #name > 253 or string.find(name, "[^a-z0-9.%-]")
        or string.find(name, "..", 1, true) then return nil end
    for label in string.gmatch(name, "[^.]+") do
        if #label > 63 or not string.match(label, "^[a-z0-9]")
            or not string.match(label, "[a-z0-9]$") then return nil end
    end
    if string.sub(name, 1, 1) == "." or string.sub(name, -1) == "." then return nil end
    return name
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
    if not normalized then return false end
    local key = resolver_key(normalized)
    if seen[key] then return true end
    seen[key] = true
    out[#out + 1] = normalized
    return true
end

local function nameservers_from_env()
    local raw = os.getenv("CUBE_EGRESS_DNS_RESOLVER_ADDRS")
    if not raw or raw == "" then return nil end
    local out, seen = {}, {}
    for token in string.gmatch(raw, "[^,%s]+") do
        if not append_nameserver(out, seen, token) then
            return nil, "invalid_proxy_dns_resolver"
        end
    end
    if #out == 0 then return nil, "invalid_proxy_dns_resolver" end
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

local function nameservers()
    if not configured_nameservers and not configuration_error then
        configured_nameservers, configuration_error = nameservers_from_env()
        if not configured_nameservers and not configuration_error then
            configured_nameservers = nameservers_from_resolv_conf()
        end
        if not configured_nameservers and not configuration_error then
            configuration_error = "no_proxy_dns_resolver"
        end
        if configured_nameservers then
            local keys = {}
            for _, ns in ipairs(configured_nameservers) do keys[#keys + 1] = resolver_key(ns) end
            -- Shared dictionaries survive reloads. A changed resolver view
            -- must not inherit entries from the previous configuration.
            cache_prefix = "dns-v2:" .. table.concat(keys, ",") .. ":"
        end
    end
    return configured_nameservers, configuration_error
end

local function clamp_ttl(ttl)
    ttl = tonumber(ttl) or 0
    if ttl < 0 then return 0 end
    if ttl > CACHE_TTL_MAX then return CACHE_TTL_MAX end
    return math.floor(ttl)
end

local function resolve_ipv4s_uncached(name)
    local ok_lib, resolver_lib = pcall(require, "resty.dns.resolver")
    if not ok_lib or not resolver_lib then
        return nil, "resolver_module_unavailable:" .. tostring(resolver_lib)
    end
    -- Resolver configuration is process-owned and read once per worker.
    local servers, config_err = nameservers()
    if not servers then return nil, config_err end
    local resolver, err = resolver_lib:new({
        nameservers = servers,
        retrans = 2,
        timeout = 2000,
    })
    if not resolver then return nil, "resolver_init_failed:" .. tostring(err) end

    local qname = name
    local visited = {[name] = true}
    local min_ttl = CACHE_TTL_MAX
    local hops = 0
    while true do
        local answers, qerr = resolver:query(qname, {qtype = resolver.TYPE_A})
        if not answers then return nil, "dns_query_failed:" .. tostring(qerr) end
        if answers.errcode then return nil, "dns_rcode_" .. tostring(answers.errcode) end

        -- Accept only IN records owned by the queried name or its CNAME
        -- chain. Unrelated answers must never authorize a destination.
        while true do
            local out, seen, cname = {}, {}, nil
            for _, ans in ipairs(answers) do
                if ans.section == 1 and ans.class == 1 and normalize_domain(ans.name) == qname then
                    if ans.type == resolver.TYPE_A then
                        local ip = parse_ipv4(ans.address)
                        if not ip then return nil, "invalid_dns_address" end
                        if not seen[ip] then out[#out + 1] = ip; seen[ip] = true end
                        min_ttl = math.min(min_ttl, clamp_ttl(ans.ttl))
                    elseif ans.type == resolver.TYPE_CNAME then
                        local target = normalize_domain(ans.cname)
                        if not target or (cname and cname ~= target) then
                            return nil, "invalid_dns_cname"
                        end
                        cname = target
                        min_ttl = math.min(min_ttl, clamp_ttl(ans.ttl))
                    end
                end
            end
            if #out > 0 then
                if cname then return nil, "conflicting_dns_cname" end
                return out, nil, min_ttl
            end
            if not cname then
                if qname == name or visited[qname] == "queried" then
                    return nil, "dns_no_a_records"
                end
                break
            end
            hops = hops + 1
            if visited[cname] then return nil, "dns_cname_loop" end
            if hops > MAX_CNAME_HOPS then return nil, "dns_cname_limit" end
            visited[cname] = true
            qname = cname
        end
        visited[qname] = "queried"
    end
end

local function cached_ipv4s(name)
    local c = cache()
    local list_key = cache_prefix .. name
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
end

local function run_lookup(name, deadline)
    -- The watchdog covers the entire CNAME chain, retries and TCP fallback,
    -- not just one socket read. Cancelling the query releases its cosockets.
    local started = ngx.now()
    local query = ngx.thread.spawn(resolve_ipv4s_uncached, name)
    local watchdog = ngx.thread.spawn(function()
        ngx.sleep(math.max(0, deadline - ngx.now()))
        return nil, "dns_deadline_exceeded"
    end)
    local ran, ips, err, ttl = ngx.thread.wait(query, watchdog)
    ngx.thread.kill(query)
    ngx.thread.kill(watchdog)
    if not ran then ips, err = nil, "dns_query_exception" end

    if ttl then ttl = math.max(0, ttl - (ngx.now() - started)) end
    return ips, err, ttl
end

local function finish_lookup(name, pending, ips, err)
    if inflight[name] ~= pending then return end
    pending.ips, pending.err = ips, err
    inflight[name] = nil
    active_lookups = active_lookups - 1
    if pending.waiters > 0 then pending.ready:post(pending.waiters) end
end

local function complete_lookup(premature, name, pending)
    if inflight[name] ~= pending then return end
    if premature or ngx.now() >= pending.deadline then
        return finish_lookup(name, pending, nil, "dns_deadline_exceeded")
    end
    local ran, ips, err, ttl
    ran, ips, err, ttl = pcall(run_lookup, name, pending.deadline)
    if not ran then ips, err = nil, "dns_query_exception" end
    if inflight[name] ~= pending then return end
    if ngx.now() >= pending.deadline then ips, err = nil, "dns_deadline_exceeded" end
    local c = cache()
    -- shared_dict uses millisecond expiry; a sub-millisecond TTL can round
    -- down to its special "never expire" value.
    if c and ips and #ips > 0 and ttl and ttl >= 0.001 then
        c:set(cache_prefix .. name, table.concat(ips, ","), ttl)
    end
    finish_lookup(name, pending, ips, err)
end

local function resolve_ipv4s(name)
    local servers, config_err = nameservers()
    if not servers then return nil, config_err end
    local cached = cached_ipv4s(name)
    if cached then return cached end

    -- A successfully registered timer can still be dropped if nginx cannot
    -- allocate its fake connection. Reap independently of callback execution.
    local now = ngx.now()
    for key, item in pairs(inflight) do
        if now >= item.deadline then
            finish_lookup(key, item, nil, "dns_deadline_exceeded")
        end
    end

    -- Timer ownership keeps shared work and slot cleanup alive even if the
    -- initiating client disconnects. Results are retained only while in flight.
    local pending = inflight[name]
    if not pending then
        if active_lookups >= MAX_INFLIGHT then return nil, "dns_concurrency_limit" end
        local ready, sem_err = require("ngx.semaphore").new()
        if not ready then return nil, "dns_semaphore_failed:" .. tostring(sem_err) end
        pending = {ready = ready, waiters = 0, deadline = now + LOOKUP_TIMEOUT}
        inflight[name] = pending
        active_lookups = active_lookups + 1
        local ok = ngx.timer.at(0, complete_lookup, name, pending)
        if not ok then
            inflight[name] = nil
            active_lookups = active_lookups - 1
            return nil, "dns_timer_unavailable"
        end
    end
    if pending.waiters >= MAX_WAITERS then return nil, "dns_waiter_limit" end
    pending.waiters = pending.waiters + 1
    local ok = pending.ready:wait(math.max(0, pending.deadline - ngx.now()))
    pending.waiters = pending.waiters - 1
    if not ok then
        if ngx.now() >= pending.deadline then
            finish_lookup(name, pending, nil, "dns_deadline_exceeded")
        end
        return nil, "dns_deadline_exceeded"
    end
    return pending.ips, pending.err
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

    local domain = normalize_domain(name)
    if not domain then return false, "g5_missing_domain_identity", {name = name} end
    local literal = parse_ipv4(domain)
    if literal then
        if literal == dst_ip then return true, nil, {name = name, ips = {literal}} end
        return false, "g5_ip_literal_mismatch", {
            name = name,
            dst_ip = dst_ip,
            resolved_ips = {literal},
        }
    end

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
    -- HTTPS forwards SNI as Host and uses SNI for upstream verification.
    -- A Host-constrained allow must authorize that same routing identity,
    -- including on shared hosting where unrelated names resolve to one IP.
    if ctx.scheme == "https" and match.host ~= nil then
        local host, sni = normalize_domain(ctx.host), normalize_domain(ctx.sni)
        if not host or not sni or host ~= sni then
            return false, "g5_host_sni_mismatch", {host = ctx.host, sni = ctx.sni}
        end
    end
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

return _M
