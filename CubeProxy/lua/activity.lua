-- activity.lua — per-sandbox "last active" stamps for CLM's auto-pause sweeper.
--
-- Cube Lifecycle Manager (CLM) polls /admin/last_active to learn which
-- sandboxes are still receiving traffic, so this dict is the entire feed for
-- the auto-pause decision. Failure modes:
--   - Empty ins_id (request bypassed sandbox routing): skip.
--   - Dict full (no_memory): we drop a key so other sandboxes continue to
--     register; auto-pause decisions about the dropped sandbox simply slip by
--     one sweep cycle.
-- All paths are non-blocking.

local _M = {}

function _M.touch(ins_id)
    if not ins_id or ins_id == "" then
        return
    end
    local active = ngx.shared and ngx.shared.cube_sandbox_last_active
    if not active then
        return
    end
    -- Coalesce sub-second writes: a single sandbox handling 1k QPS would
    -- otherwise issue 1k dict writes per second per worker. The dict is
    -- already keyed per-sandbox, so we only need a "good enough" most-
    -- recent-second timestamp for CLM's idle calculation. Skip
    -- if our last write was less than 1s ago.
    local now_ms = math.floor(ngx.now() * 1000)
    local prev = active:get(ins_id)
    if prev and (now_ms - prev) < 1000 then
        return
    end
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

return _M
