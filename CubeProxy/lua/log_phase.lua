-- log_phase.lua — runs after the response is sent.
--
-- 1. Records the access time for the access log (preserved).
-- 2. Stamps the per-sandbox "last active" timestamp into a worker-shared
--    lua dict. The CubeProxy-sidecar polls /admin/last_active to learn
--    which sandboxes are still receiving traffic, so this is the entire
--    feed for the auto-pause decision. Failure modes:
--      - Empty ngx.var.ins_id (request bypassed sandbox routing): skip.
--      - Dict full (no_memory): we drop a key so other sandboxes
--        continue to register; auto-pause decisions about the dropped
--        sandbox simply slip by one sweep cycle.
--    All paths are non-blocking; this phase MUST NOT delay log emission.

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

-- Record per-sandbox activity for the auto-pause sidecar. Sandboxes that
-- haven't opted into auto_pause are still cheap to record here — the
-- sidecar simply ignores their entries when computing pause decisions.
local ins_id = ngx.var.ins_id
if ins_id and ins_id ~= "" then
    local active = ngx.shared.cube_sandbox_last_active
    if active then
        -- Coalesce sub-second writes: a single sandbox handling 1k QPS would
        -- otherwise issue 1k dict writes per second per worker. The dict is
        -- already keyed per-sandbox, so we only need a "good enough" most-
        -- recent-second timestamp for the sidecar's idle calculation. Skip
        -- if our last write was less than 1s ago.
        local now_ms = math.floor(ngx.now() * 1000)
        local prev = active:get(ins_id)
        if (not prev) or (now_ms - prev) >= 1000 then
            local ok, err, forcible = active:set(ins_id, now_ms)
            if not ok then
                -- safe_set isn't worth the extra LOC here; on failure we just
                -- log at warn — the next request retries.
                ngx.log(ngx.WARN, "LEVEL_WARN||",
                    string.format("cube_sandbox_last_active set %s failed: %s", ins_id, tostring(err)))
            elseif forcible then
                -- Dict was full and the LRU eviction took out a different
                -- sandbox. We still recorded ours — surface visibility so
                -- the operator can grow lua_shared_dict.
                ngx.log(ngx.WARN, "LEVEL_WARN||",
                    "cube_sandbox_last_active dict full, an entry was evicted; consider raising lua_shared_dict size")
            end
        end
    end
end
