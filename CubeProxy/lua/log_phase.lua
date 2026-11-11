-- log_phase.lua — runs after the response is sent.
--
-- 1. Records the access time for the access log (preserved).
-- 2. Stamps the per-sandbox "last active" timestamp (see activity.lua).
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

-- Record per-sandbox activity for CLM's auto-pause sweeper. Sandboxes that
-- haven't opted into auto_pause are still cheap to record here — CLM
-- simply ignores their entries when computing pause decisions.
activity.touch(ngx.var.ins_id)
