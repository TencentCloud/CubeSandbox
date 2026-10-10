package.path = "lua/?.lua;" .. package.path

local records, queries, entries, clock = {}, {}, {}, 0
local query_delays = {}
ngx = {shared = {dns_auth_cache = {
    get = function(_, key)
        local item = entries[key]
        if item and item.expires > clock then return item.value end
    end,
    set = function(_, key, value, ttl)
        assert(ttl > 0, "zero TTL must never become an immortal cache entry")
        entries[key] = {value = value, expires = clock + ttl}
    end,
}}}
-- Deterministic scheduling for response-parser tests; actual scheduling and
-- concurrent lookup limits are exercised by dns_auth_integration.py.
ngx.now = function() return clock end
ngx.sleep = function() end
ngx.thread = {
    spawn = function(fn, ...) return {pcall(fn, ...)} end,
    wait = function(thread) return unpack(thread, 1, 4) end,
    kill = function() end,
}
local scheduled, dropped, drop_timer
ngx.timer = {at = function(_, fn, ...)
    if drop_timer then dropped = {fn, ...}; return true end
    scheduled = {fn, ...}
    return true
end}
package.loaded["ngx.semaphore"] = {new = function() return {
    post = function() end,
    wait = function()
        if not scheduled then clock = clock + 5; return false end
        if dropped then
            local old = dropped
            dropped = nil
            old[1](false, unpack(old, 2))
        end
        local job = scheduled
        scheduled = nil
        job[1](false, unpack(job, 2))
        return true
    end,
} end}
package.loaded["resty.dns.resolver"] = {
    new = function(_, opts)
        assert(#opts.nameservers > 0)
        return {TYPE_A = 1, TYPE_CNAME = 5, query = function(_, name)
            queries[#queries + 1] = name
            clock = clock + (query_delays[name] or 0)
            local response = records[name]
            if response == "timeout" then return nil, "timeout" end
            return response or {errcode = 3}
        end}
    end,
}
local dns_auth = require "dns_auth"
local function entry(name)
    for key, value in pairs(entries) do
        if string.sub(key, -#name - 1) == ":" .. name then return value end
    end
end
local function a(name, ip, ttl)
    return {name = name, address = ip, ttl = ttl or 30, class = 1, type = 1, section = 1}
end
local function cname(name, target, ttl)
    return {name = name, cname = target, ttl = ttl or 30, class = 1, type = 5, section = 1}
end
local function verify(name, ip, expected, reason, match, ctx)
    ctx = ctx or {host = name, dst_ip = ip, scheme = "http"}
    local ok, why = dns_auth.verify_rule(match or {host = "*.example.com"}, ctx)
    assert(ok == expected, name .. ": " .. tostring(why))
    if reason then assert(why == reason, tostring(why)) end
end

records["allowed.example.com"] = {a("allowed.example.com", "198.51.100.9"),
    a("allowed.example.com", "198.51.100.10")}
verify("allowed.example.com", "203.0.113.66", false, "g5_dst_ip_not_in_dns")
verify("ALLOWED.EXAMPLE.COM.", "198.51.100.10", true)
verify("198.51.100.9", "198.51.100.9", true)
verify("198.51.100.9", "198.51.100.10", false, "g5_ip_literal_mismatch")
verify("missing.example.com", "198.51.100.9", false, "g5_dns_resolve_failed")
records["timeout.example.com"] = "timeout"
verify("timeout.example.com", "198.51.100.9", false, "g5_dns_resolve_failed")
verify("bad..example.com", "198.51.100.9", false, "g5_missing_domain_identity")
verify("allowed.example.com", "::1", false, "g5_missing_original_dst")

records["poison.example.com"] = {a("unrelated.example.com", "203.0.113.66")}
verify("poison.example.com", "203.0.113.66", false, "g5_dns_resolve_failed")
local additional = a("additional.example.com", "203.0.113.66")
additional.section = 3
records["additional.example.com"] = {additional}
verify("additional.example.com", "203.0.113.66", false, "g5_dns_resolve_failed")
records["alias.example.com"] = {cname("alias.example.com", "target.example.net", 2),
    a("target.example.net", "198.51.100.9"), a("unrelated.example.com", "203.0.113.66")}
verify("alias.example.com", "203.0.113.66", false, "g5_dst_ip_not_in_dns")
verify("alias.example.com", "198.51.100.9", true)
assert(entry("alias.example.com").expires == 2)
clock = 3
records["alias.example.com"] = {a("alias.example.com", "198.51.100.10")}
verify("alias.example.com", "198.51.100.9", false, "g5_dst_ip_not_in_dns")

records["split.example.com"] = {cname("split.example.com", "target.example.net")}
records["target.example.net"] = {a("target.example.net", "198.51.100.9")}
verify("split.example.com", "198.51.100.9", true)
assert(queries[#queries] == "target.example.net")
records["loop.example.com"] = {cname("loop.example.com", "loop.example.com")}
verify("loop.example.com", "198.51.100.9", false, "g5_dns_resolve_failed")
for i = 1, 7 do
    records["hop" .. i .. ".example.com"] = {
        cname("hop" .. i .. ".example.com", "hop" .. (i + 1) .. ".example.com")}
end
verify("hop1.example.com", "198.51.100.9", false, "g5_dns_resolve_failed")
records["zero.example.com"] = {a("zero.example.com", "198.51.100.9", 0)}
verify("zero.example.com", "198.51.100.9", true)
assert(entry("zero.example.com") == nil)
records["zero.example.com"] = {a("zero.example.com", "198.51.100.10", 0)}
verify("zero.example.com", "198.51.100.9", false, "g5_dst_ip_not_in_dns")
records["tiny.example.com"] = {a("tiny.example.com", "198.51.100.9", 1)}
query_delays["tiny.example.com"] = 0.9999
verify("tiny.example.com", "198.51.100.9", true)
assert(entry("tiny.example.com") == nil, "sub-millisecond TTL must not become permanent")

verify("allowed.example.com", nil, false, "g5_host_sni_mismatch", {host = "allowed.example.com"},
    {host = "allowed.example.com", sni = "blocked.example.com", dst_ip = "198.51.100.9", scheme = "https"})
verify("allowed.example.com", nil, true, nil, {host = "allowed.example.com"},
    {host = "allowed.example.com", sni = "ALLOWED.EXAMPLE.COM", dst_ip = "198.51.100.9", scheme = "https"})
verify("allowed.example.com", nil, false, "g5_dst_ip_not_in_dns", {sni = "*.example.com"},
    {sni = "allowed.example.com", dst_ip = "203.0.113.66", scheme = "https"})
verify("anything.example.com", "203.0.113.66", true, nil, {})

drop_timer = true
for i = 1, 20 do
    verify("dropped" .. i .. ".example.com", "198.51.100.9", false, "g5_dns_resolve_failed")
end
drop_timer = false
records["dropped20.example.com"] = {a("dropped20.example.com", "198.51.100.9")}
verify("dropped20.example.com", "198.51.100.9", true)

local original_getenv = os.getenv
os.getenv = function(key)
    if key == "CUBE_EGRESS_DNS_RESOLVER_ADDRS" then return "invalid-resolver" end
    return original_getenv(key)
end
package.loaded.dns_auth = nil
dns_auth = require "dns_auth"
verify("allowed.example.com", "198.51.100.9", false, "g5_dns_resolve_failed")
os.getenv = function(key)
    if key == "CUBE_EGRESS_DNS_RESOLVER_ADDRS" then return "127.0.0.9:5300" end
    return original_getenv(key)
end
package.loaded.dns_auth = nil
dns_auth = require "dns_auth"
records["allowed.example.com"] = {a("allowed.example.com", "203.0.113.66")}
verify("allowed.example.com", "198.51.100.9", false, "g5_dst_ip_not_in_dns")
os.getenv = original_getenv
print("dns_auth_test: PASS (resolver responses, CNAME ownership, expiry, failure and routing boundaries)")
