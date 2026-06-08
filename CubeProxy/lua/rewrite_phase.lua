local utils = require "utils"
local sb = require "sandbox_backend"
local state = require "sandbox_state"

-- Parse Host: <container_port>-<sandbox_id>.<domain> e.g. 49983-7c8fbcd45ffe450fb8f7fb223ad45507.cube.app
-- Returns container_port, ins_id (sandbox / instance id), or nil, nil on failure.
local function parse_port_and_instance_from_host(host)
    if utils:is_null(host) then
        return nil, nil
    end
    local hostname = host:match("^([^%:]+)")
    if utils:is_null(hostname) then
        hostname = host
    end
    local container_port, ins_id = hostname:match("^(%d+)%-([^%.]+)")
    if not container_port or not ins_id or ins_id == "" then
        return nil, nil
    end
    return container_port, ins_id
end

local container_port, ins_id = parse_port_and_instance_from_host(ngx.var.http_host)
if not container_port or not ins_id then
    ngx.log(ngx.ERR, "LEVEL_WARN||",
        string.format("request %s invalid Host for port/instance parse: %s",
            ngx.var.http_x_cube_request_id, ngx.var.http_host))
    ngx.var.cube_retcode = "310400"
    ngx.exit(400)
end

-- log_phase reads ngx.var.ins_id to record activity. The host-based location
-- doesn't otherwise set it (only the path-based one does, for proxy_redirect
-- purposes), so populate it here to make activity tracking work uniformly.
ngx.var.ins_id = ins_id

-- Auto-pause gate. See sandbox_state.lua for failure-mode semantics.
state.gate(ins_id)

local host_ip, host_port = sb.resolve_backend(ins_id, container_port)
ngx.var.backend_ip = host_ip
ngx.var.backend_port = host_port
