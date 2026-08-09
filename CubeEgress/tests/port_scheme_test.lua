package.path = "lua/?.lua;" .. package.path

package.preload["cjson.safe"] = function()
    return {encode = function() return "{}" end, decode = function() return {} end}
end
package.preload["resty.openssl.digest"] = function()
    return {new = function() return nil, "not used by validation tests" end}
end

local port_scheme = require "port_scheme"
local policy = require "policy"
local access = require "access_phase"

local function assert_true(value, message)
    if not value then error(message or "expected true") end
end

local function assert_false(value, message)
    if value then error(message or "expected false") end
end

local function rule(id, match)
    return {id = id, match = match, action = {allow = true}}
end

assert_true(port_scheme.matches(nil, nil, 80, "http"))
assert_true(port_scheme.matches(nil, nil, 443, "https"))
assert_false(port_scheme.matches(nil, nil, 8080, "http"))
assert_true(port_scheme.matches(nil, "http", 80, "http"))
assert_false(port_scheme.matches(nil, "http", 8080, "http"))
assert_true(port_scheme.matches(8443, "https", 8443, "https"))
assert_false(port_scheme.matches(8443, "https", 8443, "http"))
assert_false(port_scheme.matches(8443, nil, 8443, "https"))

-- Regression: a broad host rule must not shadow the custom-port rule.
local ctx = {
    host = "api.example.com", sni = "api.example.com", method = "GET",
    path = "/", scheme = "https", dst_port = 8443,
}
assert_false(access._rule_matches({host = "api.example.com"}, ctx))
assert_true(access._rule_matches({host = "api.example.com", port = 8443, scheme = "https"}, ctx))

local function validate(rules)
    return policy.validate_policy({policy_id = "sandbox-1", rules = rules})
end

local ok, err = validate({rule("port-only", {host = "api.example.com", port = 8080})})
assert_false(ok, "port-only policy unexpectedly valid")
assert_true(string.find(err, "port requires scheme", 1, true) ~= nil)

ok = validate({
    rule("default", {host = "api.example.com"}),
    rule("conflict", {host = "api.example.com", port = 443, scheme = "http"}),
})
assert_false(ok, "default/explicit scheme conflict unexpectedly valid")

local many = {}
for i = 1, 9 do
    many[i] = rule("r" .. i, {host = "api.example.com", port = 8000 + i, scheme = "http"})
end
ok = validate(many)
assert_false(ok, "nine tuples unexpectedly valid")

ok, err = validate({
    rule("host-a", {host = "a.example.com", port = 8443, scheme = "http"}),
    rule("host-b", {host = "b.example.com", port = 8443, scheme = "https"}),
})
assert_true(ok, err)

-- normalize_identity: bare IP and /32 spelling aggregate under one identity,
-- so a per-(host,port) scheme conflict across the two spellings is caught
-- (Go l7GroupKey groups both via parseCIDR).
ok, err = validate({
    rule("ip-bare", {host = "1.2.3.4", port = 8443, scheme = "https"}),
    rule("ip-cidr32", {host = "1.2.3.4/32", port = 8443, scheme = "http"}),
})
assert_false(ok, "bare-IP vs /32 scheme conflict unexpectedly valid")
assert_true(string.find(err, "conflicts", 1, true) ~= nil)

-- Same tuple under both spellings is a no-op duplicate, not a conflict.
ok, err = validate({
    rule("ip-bare", {host = "1.2.3.4", port = 8443, scheme = "https"}),
    rule("ip-cidr32", {host = "1.2.3.4/32", port = 8443, scheme = "https"}),
})
assert_true(ok, err)

-- Subnet CIDRs are rejected as L7 hosts (mirrors Go classifyL7Target).
ok, err = validate({rule("subnet", {host = "1.2.3.0/24", port = 8443, scheme = "https"})})
assert_false(ok, "subnet CIDR host unexpectedly valid")
assert_true(string.find(err, "subnet CIDR", 1, true) ~= nil)

ok = validate({rule("bad-prefix", {host = "1.2.3.4/33", port = 8443, scheme = "https"})})
assert_false(ok, "invalid CIDR prefix unexpectedly valid")

ok = validate({rule("leading-zero", {host = "01.2.3.4", port = 8443, scheme = "https"})})
assert_false(ok, "leading-zero IPv4 unexpectedly valid")

ok = validate({rule("sni-subnet", {sni = "10.0.0.0/8", port = 443, scheme = "https"})})
assert_false(ok, "subnet CIDR sni unexpectedly valid")

-- Whitespace-padded host is rejected, not trimmed (request-time matching
-- compares the raw value, so trimming would accept a dead rule).
ok, err = validate({rule("padded", {host = "  example.com  ", port = 8443, scheme = "https"})})
assert_false(ok, "whitespace-padded host unexpectedly valid")
assert_true(string.find(err, "whitespace", 1, true) ~= nil)

-- Case + trailing-dot canonicalization still groups DNS names.
ok = validate({
    rule("dns-upper", {host = "API.example.com", port = 8443, scheme = "https"}),
    rule("dns-dot", {host = "api.example.com.", port = 8443, scheme = "http"}),
})
assert_false(ok, "case/dot DNS scheme conflict unexpectedly valid")

print("port_scheme_test: PASS")
