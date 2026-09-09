package.path = "lua/?.lua;" .. package.path

local dns_auth = require "dns_auth"

local function assert_true(value, message)
    if not value then error(message or "expected true") end
end

local function assert_false(value, message)
    if value then error(message or "expected false") end
end

local function assert_eq(got, want, message)
    if got ~= want then
        error((message or "unexpected value") .. ": got=" .. tostring(got) .. " want=" .. tostring(want))
    end
end

local records = {
    ["bypass.blob.core.windows.net"] = {"20.42.1.7"},
    ["api.example.com"] = {"198.51.100.9"},
    ["sni.example.com"] = {"198.51.100.10"},
}

dns_auth._set_test_resolver(function(name)
    local ips = records[name]
    if not ips then return nil, "test_nxdomain" end
    return ips, nil, 30
end)

local ok, reason = dns_auth.verify_rule(
    {host = "*.blob.core.windows.net", scheme = "http"},
    {
        host = "bypass.blob.core.windows.net",
        dst_ip = "203.0.113.66",
    })
assert_false(ok, "forged HTTP Host should not authorize an unrelated original dst")
assert_eq(reason, "g5_dst_ip_not_in_dns")

ok, reason = dns_auth.verify_rule(
    {host = "*.blob.core.windows.net", scheme = "http"},
    {
        host = "bypass.blob.core.windows.net",
        dst_ip = "20.42.1.7",
    })
assert_true(ok, reason)

ok, reason = dns_auth.verify_rule(
    {sni = "*.example.com", scheme = "https"},
    {
        sni = "sni.example.com",
        dst_ip = "203.0.113.66",
    })
assert_false(ok, "forged TLS SNI should not authorize an unrelated original dst")
assert_eq(reason, "g5_dst_ip_not_in_dns")

ok, reason = dns_auth.verify_rule(
    {host = "api.example.com", sni = "api.example.com", scheme = "https"},
    {
        host = "api.example.com",
        sni = "api.example.com",
        dst_ip = "198.51.100.9",
    })
assert_true(ok, reason)

ok, reason = dns_auth.verify_rule(
    {host = "198.51.100.20", scheme = "http"},
    {
        host = "198.51.100.20",
        dst_ip = "198.51.100.21",
    })
assert_false(ok, "IP-literal Host must match the original destination exactly")
assert_eq(reason, "g5_ip_literal_mismatch")

ok, reason = dns_auth.verify_rule(
    {method = {"GET"}, scheme = "http"},
    {
        host = "anything.example.com",
        dst_ip = "203.0.113.66",
    })
assert_true(ok, reason)

print("dns_auth_test: PASS")
