-- sandbox_state.lua
--
-- Auto-pause / auto-resume gate run from the rewrite phase.
--
-- Reads the per-sandbox state from cube_sandbox_state (a worker-shared dict
-- maintained by CubeProxy-sidecar over the admin server) and either:
--   - lets the request through (state == "running" or unknown / not paused),
--   - returns 503 Retry-After when the sandbox is mid-pause (avoid the race
--     where we'd resume something the sidecar is actively pausing),
--   - or fires an internal sub-request to the sidecar's /internal/resume,
--     blocking the dataplane request until the sandbox is alive again.
--
-- Failure modes are biased towards availability:
--   * Lifecycle feature disabled         -> always pass.
--   * State dict missing or empty entry  -> always pass (sidecar hasn't
--                                            gotten to us yet, which is the
--                                            same as "not opted in").
--   * Sidecar address unset              -> log + pass (paused sandbox will
--                                            return 5xx on its own; the
--                                            operator gets a clear error).
--   * Sidecar resume fails / times out   -> 503 with Retry-After to let the
--                                            client back off.

local utils = require "utils"

local _M = { _VERSION = "0.01" }

-- gate runs from rewrite phase. On a successful resume it returns to the
-- caller; everything else either passes through (running) or terminates the
-- request with ngx.exit().
--
-- The gate is always-on but cheap when the sandbox has no state entry: a
-- single shared-dict lookup and an early return. The sidecar only populates
-- cube_sandbox_state for sandboxes that opted into auto_pause, so for the
-- common (non-opted-in) case this is essentially free.
function _M.gate(ins_id)
    if not ins_id or ins_id == "" then
        return
    end

    local states = ngx.shared.cube_sandbox_state
    if not states then
        return
    end

    local state = states:get(ins_id)
    if not state or state == "running" then
        return
    end

    if state == "pausing" then
        -- Sidecar is mid-flight; better to bounce the client than race it.
        ngx.log(ngx.WARN, "LEVEL_WARN||",
            string.format("request %s sandbox %s is pausing; returning 503",
                ngx.var.http_x_cube_request_id or "-", ins_id))
        ngx.header["Retry-After"] = "2"
        utils:respond_unavailable()
    end

    if state == "killing" or state == "killed" then
        ngx.log(ngx.WARN, "LEVEL_WARN||",
            string.format("request %s sandbox %s is %s; returning 410",
                ngx.var.http_x_cube_request_id or "-", ins_id, state))
        ngx.var.cube_retcode = "310410"
        ngx.exit(410)
    end

    if state ~= "paused" then
        -- Unknown state — log once and let the request through; resume logic
        -- only fires for the well-known "paused" value to keep behaviour
        -- predictable as new states are added.
        ngx.log(ngx.WARN, "LEVEL_WARN||",
            string.format("sandbox %s has unknown state %q; passing through",
                ins_id, tostring(state)))
        return
    end

    -- state == "paused": ask the sidecar to resume, then continue.
    if not ngx.var.cube_sidecar_addr or ngx.var.cube_sidecar_addr == "" then
        ngx.log(ngx.ERR, "LEVEL_ERROR||",
            string.format("sandbox %s is paused but cube_sidecar_addr is unset; dataplane will fail",
                ins_id))
        return
    end

    -- ngx.location.capture issues a sub-request to /_sidecar_resume which
    -- proxy_passes to the sidecar. We pass sandbox_id and request_id as args
    -- so the sidecar can correlate logs and dedupe.
    local args = "sandbox_id=" .. ngx.escape_uri(ins_id)
    local rid = ngx.var.http_x_cube_request_id
    if rid and rid ~= "" then
        args = args .. "&request_id=" .. ngx.escape_uri(rid)
    end

    -- Explicitly set body="" so ngx.location.capture does NOT inherit the
    -- parent request's body. Without this, capture reuses the parent's
    -- Content-Length header (e.g. "112" from a POST /execute), then our
    -- /_sidecar_resume location's `proxy_pass_request_body off` strips
    -- the actual body but leaves the inherited Content-Length intact.
    -- The sidecar's Go http.Server then blocks reading the promised 112
    -- bytes that never arrive, parking the connection until the keepalive
    -- timeout (~15s) — even though the request itself is processed
    -- successfully on the sidecar side.
    local res = ngx.location.capture("/_sidecar_resume", {
        method = ngx.HTTP_POST,
        args = args,
        body = "",
    })

    if not res or res.status ~= ngx.HTTP_OK then
        local status = (res and res.status) or "nil"
        ngx.log(ngx.ERR, "LEVEL_ERROR||",
            string.format("request %s sidecar resume for sandbox %s failed: status=%s body=%s",
                rid or "-", ins_id, tostring(status),
                (res and res.body) and string.sub(res.body, 1, 200) or "-"))
        ngx.header["Retry-After"] = "5"
        utils:respond_unavailable()
    end

    -- Resume succeeded. Optimistically mark the sandbox running locally so a
    -- burst of concurrent requests that arrived during the pause don't all
    -- launch their own resume sub-requests. The sidecar will push the
    -- authoritative value via /admin/state shortly after, which simply
    -- overwrites this.
    states:set(ins_id, "running")
end

return _M
