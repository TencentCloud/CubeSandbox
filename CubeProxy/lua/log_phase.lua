-- log_phase.lua — runs after the response is sent.
--
-- 1. Records the access time for the access log (preserved).
-- 2. Finishes per-sandbox activity tracking (see activity.lua): decrements
--    the in-flight counter opened in rewrite and restamps last_active.
--    All paths are non-blocking; this phase MUST NOT delay log emission.

local activity = require "activity"

local function get_currtime()
    -- ngx.var.msec = 1663839717.105
    local current_time_seconds_with_ms = ngx.var.msec

    -- Get milliseconds part
    local current_ms = math.floor((current_time_seconds_with_ms * 1000) % 1000)

    -- Format
    local formatted_time = os.date("%Y-%m-%dT%H:%M:%S") .. "." .. current_ms
    return formatted_time
end

ngx.var.access_time = get_currtime()

-- Close out the in-flight request opened at rewrite. Sandboxes that haven't
-- opted into auto_pause are still cheap to record here — CLM simply ignores
-- their entries when computing pause decisions.
activity.finish(ngx.var.ins_id)
