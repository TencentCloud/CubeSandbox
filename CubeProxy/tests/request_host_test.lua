package.path = "./lua/?.lua;" .. package.path

local request_host = require "request_host"

local function expect(mask, port, original, want_host, want_applied)
    local got_host, got_applied, err = request_host.render(mask, port, original)
    assert(err == nil, tostring(err))
    assert(got_host == want_host,
        string.format("host mismatch: got=%s want=%s", tostring(got_host), tostring(want_host)))
    assert(got_applied == want_applied,
        string.format("applied mismatch: got=%s want=%s", tostring(got_applied), tostring(want_applied)))
end

expect(nil, "3000", "3000-sandbox.cube.app", "3000-sandbox.cube.app", false)
expect("localhost:${PORT}", "3000", "3000-sandbox.cube.app", "localhost:3000", true)
expect("localhost:${PORT}", "03000", "03000-sandbox.cube.app", "localhost:3000", true)
expect("x-${PORT}:${PORT}", "8080", "8080-sandbox.cube.app", "x-8080:8080", true)
expect("[::1]:${PORT}", "8080", "8080-sandbox.cube.app", "[::1]:8080", true)
expect("localhost:${PORT}", "49983", "49983-sandbox.cube.app", "49983-sandbox.cube.app", false)
expect("localhost:${PORT}", "049983", "049983-sandbox.cube.app", "049983-sandbox.cube.app", false)

local forwarded_headers = {}
ngx = {
    ERR = "ERR",
    var = {
        http_host = "3000-sandbox.cube.app",
        http_x_cube_request_id = "request-1",
        ins_id = "sandbox",
        cube_upstream_host = "",
    },
    req = {
        set_header = function(name, value)
            forwarded_headers[name] = value
        end,
    },
    log = function() end,
}

request_host.apply("localhost:${PORT}", "3000")
assert(ngx.var.cube_upstream_host == "localhost:3000")
assert(forwarded_headers["X-Forwarded-Host"] == "3000-sandbox.cube.app")

forwarded_headers = {}
ngx.var.cube_upstream_host = ""
request_host.apply(nil, "3000")
assert(ngx.var.cube_upstream_host == "3000-sandbox.cube.app")
assert(forwarded_headers["X-Forwarded-Host"] == nil)

print("request_host tests passed")
