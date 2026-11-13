-- activity.lua — per-sandbox activity stamps + in-flight request counts for
-- CLM's auto-pause sweeper.
--
-- Cube Lifecycle Manager (CLM) polls /admin/last_active to learn which
-- sandboxes are still receiving traffic, so this dict is the entire feed for
-- the auto-pause decision.
--
-- A request is active from rewrite (begin) until log_phase (finish). While the
-- in-flight count is > 0, /admin/last_active reports "now" for that sandbox so
-- long-lived traffic (streaming RPCs, WebSocket upgrades, …) is not treated as
-- idle mid-request. Failure modes:
--   - Empty ins_id (request bypassed sandbox routing): skip.
--   - Dict full (no_memory / forcible eviction): we log and continue; an
--     evicted live counter can briefly look idle until the next begin.
--   - Missed finish (worker SIGKILL/OOM, rare paths that skip log_by_lua):
--     the counter stays > 0 with no TTL. Recovery is admin meta/delete
--     (clear) or a CubeProxy restart — there is no mid-flight heartbeat that
--     could age out a still-open stream without false-pausing it.
-- All paths are non-blocking.

local _M = {}

-- Begin marker lives in both ngx.ctx (fast, rewrite-retry idempotency) and
-- ngx.var $cube_activity_begun. error_page → named-location redirects clear
-- ngx.ctx but keep request variables, so finish would otherwise miss the
-- begin and leave the inflight counter stuck forever.
local CTX_BEGUN = "cube_activity_begun"
local VAR_BEGUN = "cube_activity_begun"

local function last_dict()
    return ngx.shared and ngx.shared.cube_sandbox_last_active
end

local function inflight_dict()
    return ngx.shared and ngx.shared.cube_sandbox_inflight
end

local function get_begun()
    local v = ngx.var and ngx.var[VAR_BEGUN]
    if v and v ~= "" then
        return v
    end
    return ngx.ctx and ngx.ctx[CTX_BEGUN] or nil
end

local function set_begun(ins_id)
    -- ngx.var write needs `set $cube_activity_begun ""` in nginx.conf; pcall
    -- keeps unit tests that only mock ngx.ctx working.
    if ngx.var then
        pcall(function()
            ngx.var[VAR_BEGUN] = ins_id or ""
        end)
    end
    if ngx.ctx then
        ngx.ctx[CTX_BEGUN] = ins_id
    end
end

local function clear_begun()
    if ngx.var then
        pcall(function()
            ngx.var[VAR_BEGUN] = ""
        end)
    end
    if ngx.ctx then
        ngx.ctx[CTX_BEGUN] = nil
    end
end

function _M.touch(ins_id)
    if not ins_id or ins_id == "" then
        return
    end
    local active = last_dict()
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

function _M.inflight(ins_id)
    if not ins_id or ins_id == "" then
        return 0
    end
    local dict = inflight_dict()
    if not dict then
        return 0
    end
    return tonumber(dict:get(ins_id)) or 0
end

-- begin marks the start of an admitted dataplane request. Idempotent per
-- request via the begun marker so rewrite retries do not double-count. The
-- marker is only set when the counter actually increments so finish cannot
-- decrement a live key that belongs to another request after a failed incr.
function _M.begin(ins_id)
    if not ins_id or ins_id == "" then
        return
    end
    if get_begun() then
        return
    end
    local dict = inflight_dict()
    if dict then
        local newv, err, forcible = dict:incr(ins_id, 1, 0)
        if not newv then
            ngx.log(ngx.WARN, "LEVEL_WARN||",
                string.format("cube_sandbox_inflight incr %s failed: %s",
                    ins_id, tostring(err)))
        else
            if forcible then
                ngx.log(ngx.WARN, "LEVEL_WARN||",
                    "cube_sandbox_inflight dict full, an entry was evicted; consider raising lua_shared_dict size")
            end
            set_begun(ins_id)
        end
    else
        -- No shared dict (unit tests / misconfig): still mark begun so finish
        -- stays idempotent and last_active touch below still runs.
        set_begun(ins_id)
    end
    _M.touch(ins_id)
end

-- finish marks request completion (log_phase). Only decrements when begin ran
-- on this request. Always restamps last_active when ins_id is present so
-- completed traffic still resets the idle clock.
--
-- Decrement is a single atomic incr(-1) with no init. Do NOT delete when the
-- counter hits 0: a concurrent begin can incr 0→1 between our incr and a
-- delete, and the delete would erase that live request. Zero keys are inert
-- (reported_active_ms only treats > 0 as open) and are reclaimed by clear()
-- on sandbox delete; begin's incr(..., 1, 0) reuses them.
function _M.finish(ins_id)
    local begun = get_begun()
    if not ins_id or ins_id == "" then
        ins_id = begun
    end
    if not ins_id or ins_id == "" then
        return
    end
    if begun and begun == ins_id then
        local dict = inflight_dict()
        if dict then
            -- No init: missing key means nothing to finish (evicted / cleared).
            local newv, err, forcible = dict:incr(ins_id, -1)
            if newv == nil then
                if err and err ~= "not found" then
                    ngx.log(ngx.WARN, "LEVEL_WARN||",
                        string.format("cube_sandbox_inflight decr %s failed: %s",
                            ins_id, tostring(err)))
                end
            else
                if forcible then
                    ngx.log(ngx.WARN, "LEVEL_WARN||",
                        "cube_sandbox_inflight dict full, an entry was evicted; consider raising lua_shared_dict size")
                end
                if newv < 0 then
                    -- Should not happen; drop a corrupt counter rather than leave
                    -- a permanently "active" negative or wedged value.
                    dict:delete(ins_id)
                end
            end
        end
        clear_begun()
    end
    _M.touch(ins_id)
end

function _M.clear(ins_id)
    if not ins_id or ins_id == "" then
        return
    end
    local dict = inflight_dict()
    if dict then
        dict:delete(ins_id)
    end
    local active = last_dict()
    if active then
        active:delete(ins_id)
    end
end

-- reported_active_ms is what /admin/last_active should expose for CLM.
-- Open in-flight requests count as "active right now".
function _M.reported_active_ms(ins_id, stored_ms, now_ms)
    if _M.inflight(ins_id) > 0 then
        return now_ms
    end
    return stored_ms
end

return _M
