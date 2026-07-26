-- request_host.lua
--
-- Renders the per-sandbox upstream Host template after routing has already
-- parsed the original public Host/path. The envd data plane keeps its original
-- Host, matching E2B's maskRequestHost contract.
--
-- Shape/safety of maskRequestHost is enforced at create time (CubeAPI /
-- CubeMaster). This module only applies the stored template at request time.

local utils = require "utils"

local _M = { _VERSION = "0.01" }

local ENVD_PORT = 49983
local PORT_PLACEHOLDER_PATTERN = "%$%{PORT%}"

function _M.render(mask, container_port, original_host)
    if utils:is_null(mask) then
        return original_host, false, nil
    end

    -- Routing already parsed digits; tonumber normalizes leading zeros for
    -- ${PORT} expansion and for the envd exemption check.
    local port = tonumber(container_port)
    if port == ENVD_PORT then
        return original_host, false, nil
    end

    local port_text = port and tostring(port) or tostring(container_port)
    local rendered, _ = mask:gsub(PORT_PLACEHOLDER_PATTERN, port_text)
    return rendered, true, nil
end

function _M.apply(mask, container_port)
    local original_host = ngx.var.http_host
    local upstream_host, applied = _M.render(mask, container_port, original_host)
    ngx.var.cube_upstream_host = upstream_host
    if applied then
        ngx.req.set_header("X-Forwarded-Host", original_host)
    end
end

return _M
