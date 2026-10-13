package.path = "./lua/?.lua;" .. package.path

-- rewrite_phase.lua is a top-level phase script, so every case re-runs it
-- via dofile against a freshly mocked ngx.

local resolved = {} -- recorded resolve_backend(ins_id, container_port) calls
local gated = {}    -- recorded state.gate(ins_id) calls
local response_body

package.loaded["sandbox_backend"] = {
    resolve_backend = function(ins_id, container_port)
        resolved[#resolved + 1] = { ins_id = ins_id, container_port = container_port }
        return "192.168.0.10", "30000", nil
    end,
}
package.loaded["sandbox_state"] = {
    gate = function(ins_id)
        gated[#gated + 1] = ins_id
    end,
}
package.loaded["request_host"] = {
    ENVD_PORT = 49983, -- mirrors lua/request_host.lua
    apply = function(_mask, _container_port) end,
}

ngx = {
    ERR = "ERR",
    var = {},
    header = {},
    status = 0,
    log = function() end,
    say = function(body)
        response_body = body
    end,
    exit = function(status)
        error({ status = status }, 0)
    end,
}

local function run(case)
    resolved = {}
    gated = {}
    response_body = nil
    ngx.var = {
        http_x_cube_request_id = "test-request",
        http_host = case.host,
        http_e2b_sandbox_id = case.sandbox_id,
        http_e2b_sandbox_port = case.sandbox_port,
    }
    return pcall(dofile, "lua/rewrite_phase.lua")
end

local function expect_route(case, want_id, want_port, message)
    local ok, err = run(case)
    assert(ok, message .. ": phase script raised: " .. tostring(err))
    assert(#resolved == 1, message .. ": backend must be resolved exactly once")
    assert(resolved[1].ins_id == want_id,
        string.format("%s: ins_id mismatch: got=%s want=%s",
            message, tostring(resolved[1].ins_id), tostring(want_id)))
    assert(resolved[1].container_port == want_port,
        string.format("%s: container_port mismatch: got=%s want=%s",
            message, tostring(resolved[1].container_port), tostring(want_port)))
    assert(#gated == 1 and gated[1] == want_id,
        string.format("%s: gate must see the routed ins_id: got=%s want=%s",
            message, tostring(gated[1]), tostring(want_id)))
end

local function expect_bad_request(case, message)
    local ok, err = run(case)
    assert(not ok, message .. ": request must terminate through ngx.exit")
    assert(type(err) == "table" and err.status == 400,
        message .. ": unexpected response status")
    assert(response_body == '{"error":"bad request"}',
        message .. ": unexpected response body")
    assert(#resolved == 0, message .. ": backend must not be resolved")
    assert(#gated == 0, message .. ": gate must not run for unroutable requests")
end

-- Host-based routing keeps working exactly as before.
expect_route(
    { host = "49983-7c8fbcd45ffe450fb8f7fb223ad45507.cube.app" },
    "7c8fbcd45ffe450fb8f7fb223ad45507", "49983",
    "host route")

-- Host wins when both Host and headers carry routing attributes.
expect_route(
    { host = "3000-hostid.cube.app", sandbox_id = "header-id", sandbox_port = "49983" },
    "hostid", "3000",
    "host priority")

-- Fixed-address access (proxy IP as Host): route falls back to the E2B headers.
expect_route(
    { host = "10.0.0.1:8080", sandbox_id = "7c8fbcd", sandbox_port = "49983" },
    "7c8fbcd", "49983",
    "header route")

-- E2b-Sandbox-Port is optional and defaults to the envd port 49983.
expect_route(
    { host = "10.0.0.1", sandbox_id = "7c8fbcd" },
    "7c8fbcd", "49983",
    "default envd port")

-- An explicitly empty E2b-Sandbox-Port is null and falls back to the default.
expect_route(
    { host = "10.0.0.1", sandbox_id = "7c8fbcd", sandbox_port = "" },
    "7c8fbcd", "49983",
    "empty envd port header")

-- Sandbox ids used by namespace-scoped backends (e.g. namespace--name) pass.
expect_route(
    { host = "10.0.0.1", sandbox_id = "default--my-sandbox", sandbox_port = "3000" },
    "default--my-sandbox", "3000",
    "namespace-scoped id")

-- Neither Host nor headers carry routing attributes.
expect_bad_request({ host = "10.0.0.1:8080" }, "no routing attributes")

-- Malformed E2b-Sandbox-Port values are rejected with 400.
expect_bad_request(
    { host = "10.0.0.1", sandbox_id = "7c8fbcd", sandbox_port = "abc" },
    "non-numeric sandbox port")
expect_bad_request(
    { host = "10.0.0.1", sandbox_id = "7c8fbcd", sandbox_port = "HostIP" },
    "metadata-field sandbox port collision")
expect_bad_request(
    { host = "10.0.0.1", sandbox_id = "7c8fbcd", sandbox_port = "49983x" },
    "trailing-junk sandbox port")
expect_bad_request(
    { host = "10.0.0.1", sandbox_id = "7c8fbcd", sandbox_port = "%1" },
    "gsub-pattern sandbox port")

-- Header ids with ':' or '.' are rejected with 400.
expect_bad_request(
    { host = "10.0.0.1", sandbox_id = "evil:id", sandbox_port = "49983" },
    "colon in sandbox id")
expect_bad_request(
    { host = "10.0.0.1", sandbox_id = "evil.id", sandbox_port = "49983" },
    "dot in sandbox id")

print("rewrite_phase route tests passed")
