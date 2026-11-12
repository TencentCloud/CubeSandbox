package.path = "./lua/?.lua;" .. package.path

local last_store = {}
local inflight_store = {}

local function make_dict(store)
    local dict = {}
    function dict:get(key) return store[key] end
    function dict:set(key, value)
        store[key] = value
        return true, nil, false
    end
    function dict:delete(key) store[key] = nil end
    function dict:incr(key, delta, init)
        local cur = store[key]
        if cur == nil then
            -- Mirror OpenResty: no init → not found (finish uses this path).
            if init == nil then
                return nil, "not found", false
            end
            cur = init
        end
        cur = cur + delta
        store[key] = cur
        return cur, nil, false
    end
    return dict
end

local now = 1000.0
ngx = {
    WARN = "WARN",
    shared = {
        cube_sandbox_last_active = make_dict(last_store),
        cube_sandbox_inflight = make_dict(inflight_store),
    },
    ctx = {},
    -- Mirrors `set $cube_activity_begun ""` in nginx.conf.
    var = { cube_activity_begun = "" },
    now = function() return now end,
    log = function() end,
}

local activity = require "activity"

activity.touch("sb-1")
assert(last_store["sb-1"] == 1000000, "first touch stamps now")

now = 1000.5
activity.touch("sb-1")
assert(last_store["sb-1"] == 1000000, "sub-second touches are coalesced")

now = 1001.2
activity.touch("sb-1")
assert(last_store["sb-1"] == 1001200, "touch after 1s restamps")

activity.touch("")
activity.touch(nil)
assert(last_store[""] == nil, "empty ins_id is ignored")

-- begin/finish keep an in-flight counter for the request lifetime.
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
now = 2000.0
activity.begin("sb-live")
assert(inflight_store["sb-live"] == 1, "begin increments inflight")
assert(last_store["sb-live"] == 2000000, "begin stamps last_active")
assert(ngx.var.cube_activity_begun == "sb-live", "begin records var marker")
assert(activity.inflight("sb-live") == 1, "inflight accessor")
assert(activity.reported_active_ms("sb-live", 1, 9999999) == 9999999,
    "open inflight reports now")

activity.begin("sb-live")
assert(inflight_store["sb-live"] == 1, "begin is idempotent per request")

now = 2002.0
activity.finish("sb-live")
assert(inflight_store["sb-live"] == 0, "finish leaves zero key (no delete race)")
assert(last_store["sb-live"] == 2002000, "finish restamps last_active")
assert(ngx.var.cube_activity_begun == "", "finish clears var marker")
assert(activity.reported_active_ms("sb-live", 2002000, 9999999) == 2002000,
    "idle sandbox reports stored stamp")
-- Zero key is reused by a later begin instead of racing a delete.
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.begin("sb-live")
assert(inflight_store["sb-live"] == 1, "begin reuses zero key")
activity.finish("sb-live")
assert(inflight_store["sb-live"] == 0, "second finish back to zero")

-- Concurrent requests on the same sandbox stack the counter.
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.begin("sb-multi")
local first_ctx = ngx.ctx
local first_var = ngx.var
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.begin("sb-multi")
assert(inflight_store["sb-multi"] == 2, "second begin stacks inflight")
ngx.ctx = first_ctx
ngx.var = first_var
activity.finish("sb-multi")
assert(inflight_store["sb-multi"] == 1, "finish decrements one slot")
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
-- finish without begin on this ctx must not underflow.
activity.finish("sb-multi")
assert(inflight_store["sb-multi"] == 1, "finish without begin leaves counter")

-- error_page → @named_location clears ngx.ctx but keeps ngx.var. finish must
-- still see the begun marker via $cube_activity_begun and decrement.
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.begin("sb-redir")
assert(inflight_store["sb-redir"] == 1, "redir case begin")
assert(ngx.var.cube_activity_begun == "sb-redir", "redir case var set")
ngx.ctx = {} -- simulate error_page wiping ngx.ctx
activity.finish("sb-redir")
assert(inflight_store["sb-redir"] == 0, "finish after ctx wipe still decrements")
assert(ngx.var.cube_activity_begun == "", "redir case var cleared")

activity.clear("sb-multi")
assert(inflight_store["sb-multi"] == nil, "clear drops inflight")
assert(last_store["sb-multi"] == nil, "clear drops last_active")

-- finish tolerates a missing key (evicted / cleared) without recreating -1.
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.begin("sb-evict")
assert(inflight_store["sb-evict"] == 1, "evict case begin")
inflight_store["sb-evict"] = nil
activity.finish("sb-evict")
assert(inflight_store["sb-evict"] == nil, "finish on missing key stays absent")

-- Failed incr must not mark begun (would let finish decrement another request).
local broken = make_dict({})
function broken:incr()
    return nil, "no memory"
end
ngx.shared.cube_sandbox_inflight = broken
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.begin("sb-oom")
assert(ngx.ctx.cube_activity_begun == nil, "failed incr leaves CTX unset")
assert(ngx.var.cube_activity_begun == "", "failed incr leaves var unset")
activity.finish("sb-oom")
assert(inflight_store["sb-oom"] == nil, "failed-begin finish is a no-op")
ngx.shared.cube_sandbox_inflight = make_dict(inflight_store)

local saved_shared = ngx.shared
ngx.shared = {}
ngx.ctx = {}
ngx.var = { cube_activity_begun = "" }
activity.touch("sb-2") -- missing dict must not raise
activity.begin("sb-2")
activity.finish("sb-2")
ngx.shared = saved_shared

print("activity_test: ok")
