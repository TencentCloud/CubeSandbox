package.path = "./lua/?.lua;" .. package.path

-- path_rewrite_phase.lua is a top-level phase script, so every case re-runs
-- it via dofile against a freshly mocked ngx.

local resolved, gated, begun, upstream_uri

package.loaded["utils"] = {
    respond_bad_request = function()
        error({ status = 400 }, 0)
    end,
}
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
package.loaded["activity"] = {
    begin = function(ins_id)
        begun[#begun + 1] = ins_id
    end,
}
package.loaded["request_host"] = {
    apply = function(_mask, _container_port) end,
}

local function run(uri)
    resolved, gated, begun, upstream_uri = {}, {}, {}, nil
    ngx = {
        ERR = "ERR",
        var = { uri = uri, http_x_cube_request_id = "test-request" },
        log = function() end,
        req = {
            set_uri = function(u, _jump)
                upstream_uri = u
            end,
        },
    }
    return pcall(dofile, "lua/path_rewrite_phase.lua")
end

-- A routed request is gated, begun at request start, then resolved.
local ok, err = run("/sandbox/7c8fbcd/49983/files?path=/tmp")
assert(ok, "routed request raised: " .. tostring(err))
assert(upstream_uri == "/files?path=/tmp", "unexpected upstream uri: " .. tostring(upstream_uri))
assert(#gated == 1 and gated[1] == "7c8fbcd", "gate must see the routed ins_id")
assert(#begun == 1 and begun[1] == "7c8fbcd", "activity must begin at request start")
assert(#resolved == 1 and resolved[1].container_port == "49983", "backend must be resolved once")

-- A malformed path is rejected before the gate and must not begin activity.
ok, err = run("/sandbox/evil.id/49983/")
assert(not ok and type(err) == "table" and err.status == 400, "malformed path must return 400")
assert(#gated == 0 and #begun == 0 and #resolved == 0, "malformed path must not gate, begin or resolve")

print("path_rewrite_phase route tests passed")
