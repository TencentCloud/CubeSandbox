-- file name: path_rewrite_phase.lua
--
-- Path-based sandbox routing. Accepts URIs shaped as:
--     /sandbox/<sandbox-id>/<container-port>(/<rest>)?
-- and rewrites the upstream URI to "/<rest>" (preserving the query string),
-- then resolves the same Redis-backed backend metadata used by host-mode
-- routing in rewrite_phase.lua.

local utils = require "utils"
local sb = require "sandbox_backend"
local state = require "sandbox_state"

local uri = ngx.var.uri or ""
local ins_id, container_port, rest = uri:match("^/sandbox/([%w_%-]+)/(%d+)(/?.*)$")
if not ins_id or not container_port then
    ngx.log(ngx.ERR, "LEVEL_WARN||",
        string.format("request %s invalid path for sandbox/<id>/<port> parse: %s",
            ngx.var.http_x_cube_request_id, uri))
    utils:respond_bad_request()
end

if rest == nil or rest == "" then
    rest = "/"
end

-- Expose parsed values so proxy_redirect / proxy_cookie_path in nginx.conf
-- can rewrite Location headers and cookie Path values back into the
-- /sandbox/<id>/<port>/ prefix on the response side.
ngx.var.ins_id = ins_id
ngx.var.container_port = container_port

-- Strip the /sandbox/<id>/<port> prefix from the upstream URI. The second
-- argument is false so nginx does NOT trigger an internal redirect, letting
-- the remaining phases (balancer / header_filter / log) of this location
-- continue to run as configured.
ngx.req.set_uri(rest, false)

-- Auto-pause gate: if the sandbox is currently paused, ask the sidecar to
-- resume it before we attempt backend resolution. No-op when the lifecycle
-- feature is disabled or the sandbox isn't tracked.
state.gate(ins_id)

local host_ip, host_port = sb.resolve_backend(ins_id, container_port)
ngx.var.backend_ip = host_ip
ngx.var.backend_port = host_port
