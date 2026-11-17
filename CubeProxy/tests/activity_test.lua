package.path = "./lua/?.lua;" .. package.path

local store = {}
local dict = {}
function dict:get(key) return store[key] end
function dict:set(key, value) store[key] = value; return true, nil, false end

local now = 1000.0
ngx = {
    WARN = "WARN",
    shared = { cube_sandbox_last_active = dict },
    now = function() return now end,
    log = function() end,
}

local activity = require "activity"

activity.touch("sb-1")
assert(store["sb-1"] == 1000000, "first touch stamps now")

now = 1000.5
activity.touch("sb-1")
assert(store["sb-1"] == 1000000, "sub-second touches are coalesced")

now = 1001.2
activity.touch("sb-1")
assert(store["sb-1"] == 1001200, "touch after 1s restamps")

activity.touch("")
activity.touch(nil)
assert(store[""] == nil, "empty ins_id is ignored")

ngx.shared = {}
activity.touch("sb-2") -- missing dict must not raise

print("activity_test: ok")
