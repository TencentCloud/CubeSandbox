-- Complementary tests for CubeEgress custom-port semantics.
-- Focuses on port_scheme.expand (the branch table behind every
-- rule match) which port_scheme_test.lua exercises only indirectly
-- via port_scheme.matches. Run: lua tests/port_scheme_extra_test.lua
package.path = "lua/?.lua;" .. package.path

local port_scheme = require "port_scheme"

local function assert_true(value, message)
    if not value then error(message or "expected true") end
end

local function assert_false(value, message)
    if value then error(message or "expected false") end
end

-- Find a tuple by port in an expand() result.
local function tuple_for(tuples, port)
    if not tuples then return nil end
    for _, t in ipairs(tuples) do
        if t.port == port then return t end
    end
    return nil
end

-- Default rule (no port/scheme) -> {80/http, 443/https}.
local def, err = port_scheme.expand(nil, nil)
assert_false(err, "expand(nil, nil) should succeed")
assert_true(#def == 2, "default should yield 2 tuples, got " .. #def)
assert_true(tuple_for(def, 80) and tuple_for(def, 80).scheme == "http", "default missing 80/http")
assert_true(tuple_for(def, 443) and tuple_for(def, 443).scheme == "https", "default missing 443/https")

-- scheme-only http -> {80/http}.
local only_http = port_scheme.expand(nil, "http")
assert_true(#only_http == 1 and only_http[1].port == 80 and only_http[1].scheme == "http",
    "scheme=http should yield exactly {80/http}")

-- scheme-only https -> {443/https}.
local only_https = port_scheme.expand(nil, "https")
assert_true(#only_https == 1 and only_https[1].port == 443 and only_https[1].scheme == "https",
    "scheme=https should yield exactly {443/https}")

-- explicit custom port + scheme -> exact single tuple.
local custom = port_scheme.expand(8080, "http")
assert_true(#custom == 1 and custom[1].port == 8080 and custom[1].scheme == "http",
    "port=8080+scheme=http should yield exactly {8080/http}")
local custom_https = port_scheme.expand(8443, "https")
assert_true(#custom_https == 1 and custom_https[1].port == 8443 and custom_https[1].scheme == "https",
    "port=8443+scheme=https should yield exactly {8443/https}")

-- port without scheme is invalid.
local bad, bad_err = port_scheme.expand(8080, nil)
assert_false(bad, "port without scheme must be rejected")
assert_true(bad_err and string.find(bad_err, "port requires scheme", 1, true) ~= nil,
    "port-without-scheme error must mention 'port requires scheme'")

-- unknown scheme is invalid.
local bad_scheme, bs_err = port_scheme.expand(nil, "ftp")
assert_false(bad_scheme, "scheme=ftp must be rejected")
assert_true(bs_err and string.find(bs_err, "scheme must be http or https", 1, true) ~= nil,
    "unknown scheme error must mention allowed values")

-- out-of-range ports are invalid.
local zero, z_err = port_scheme.expand(0, "http")
assert_false(zero, "port=0 must be rejected")
local too_big, tb_err = port_scheme.expand(70000, "http")
assert_false(too_big, "port=70000 must be rejected")

-- scheme normalization is case/whitespace insensitive.
local upper = port_scheme.expand(nil, "HTTPS")
assert_true(#upper == 1 and upper[1].port == 443 and upper[1].scheme == "https",
    "scheme='HTTPS' should normalize to https/443")
local spaced = port_scheme.expand(nil, "  http  ")
assert_true(#spaced == 1 and spaced[1].port == 80 and spaced[1].scheme == "http",
    "scheme='  http  ' should normalize to http/80")

-- matches() honors case-insensitive scheme on the request too.
assert_true(port_scheme.matches(nil, "HTTPS", 443, "HTTPS"),
    "matches must accept case-insensitive scheme match")

print("port_scheme_extra_test: PASS")
