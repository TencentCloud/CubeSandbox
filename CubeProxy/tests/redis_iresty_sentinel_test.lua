-- Unit coverage for Sentinel failover recovery in redis_iresty.lua.
-- Simulates a reachable demoted master that rejects writes with READONLY,
-- then verifies a single re-resolve via Sentinel succeeds (command + pipeline).

package.path = "./lua/?.lua;" .. package.path

local cache_store = {}
local dict = {}
function dict:get(key)
    return cache_store[key]
end
function dict:set(key, value, _ttl)
    cache_store[key] = value
    return true
end
function dict:delete(key)
    cache_store[key] = nil
end

ngx = {
    null = {},
    WARN = "WARN",
    shared = { redis_sentinel_master = dict },
    log = function() end,
}

local state = {
    set_calls = 0,
    pipeline_commits = 0,
    connect_targets = {},
    -- When true, Sentinel keeps advertising the demoted master (10.0.0.1).
    sticky_demoted = false,
}

local function new_redis_obj()
    local obj = {
        _pipeline = nil,
        _host = nil,
    }
    function obj:set_timeout() end
    function obj:connect(host, port)
        self._host = host
        table.insert(state.connect_targets, host .. ":" .. tostring(port))
        return true
    end
    function obj:auth()
        return true
    end
    function obj:select()
        return true
    end
    function obj:set_keepalive()
        return true
    end
    function obj:close() end
    function obj:sentinel(_cmd, _name)
        if state.sticky_demoted then
            return { "10.0.0.1", "6379" }
        end
        -- After failover Sentinel points at the new master.
        return { "10.0.0.2", "6379" }
    end
    function obj:init_pipeline()
        self._pipeline = {}
    end
    function obj:commit_pipeline()
        state.pipeline_commits = state.pipeline_commits + 1
        local pipe = rawget(self, "_pipeline") or {}
        local results = {}
        for _, item in ipairs(pipe) do
            if item.cmd == "set" then
                state.set_calls = state.set_calls + 1
                if self._host == "10.0.0.1" then
                    results[#results + 1] = { false, "READONLY You can't write against a read only replica." }
                else
                    results[#results + 1] = "OK"
                end
            else
                results[#results + 1] = "OK"
            end
        end
        self._pipeline = nil
        return results
    end
    setmetatable(obj, {
        __index = function(_t, cmd)
            -- Only synthesize Redis command methods; never shadow fields.
            if type(cmd) ~= "string" or cmd:sub(1, 1) == "_" then
                return nil
            end
            return function(self, ...)
                local pipe = rawget(self, "_pipeline")
                if pipe then
                    table.insert(pipe, { cmd = cmd, args = { ... } })
                    return true
                end
                if cmd == "set" then
                    state.set_calls = state.set_calls + 1
                    if self._host == "10.0.0.1" then
                        return nil, "READONLY You can't write against a read only replica."
                    end
                    return "OK"
                end
                if cmd == "get" then
                    return "v"
                end
                return "OK"
            end
        end,
    })
    return obj
end

package.loaded["resty.redis"] = {
    new = function()
        return new_redis_obj()
    end,
}

local redis_iresty = require "redis_iresty"

-- split_host_port contract
local h, p = redis_iresty._split_host_port("10.0.0.11", 26379)
assert(h == "10.0.0.11" and p == 26379)
h, p = redis_iresty._split_host_port("10.0.0.11:26379", 26379)
assert(h == "10.0.0.11" and p == 26379)
h, p = redis_iresty._split_host_port("sentinel.example", 26379)
assert(h == "sentinel.example" and p == 26379)
h, p = redis_iresty._split_host_port("[2001:db8::10]:26379", 26379)
assert(h == "2001:db8::10" and p == 26379)
h, p = redis_iresty._split_host_port("[::1]", 26379)
assert(h == "::1" and p == 26379)

assert(redis_iresty._is_failover_err("READONLY You can't write against a read only replica."))
-- Ambiguous socket errors must not auto-retry (risk of double-apply).
assert(not redis_iresty._is_failover_err("connection refused"))
assert(not redis_iresty._is_failover_err("closed"))
assert(not redis_iresty._is_failover_err("WRONGTYPE"))

local client = redis_iresty:new({
    timeout = 1,
    redis_pd = "pwd",
    redis_master_name = "mymaster",
    redis_sentinel_nodes = "10.0.0.11:26379",
})

-- Seed cache with the reachable demoted master.
cache_store["mymaster@10.0.0.11:26379"] = "10.0.0.1:6379"

state.set_calls = 0
state.connect_targets = {}
local ok, err = client:set("k", "v")
assert(ok == "OK", "expected OK after READONLY retry, got " .. tostring(ok) .. " err=" .. tostring(err))
assert(state.set_calls == 2, "expected one failed SET + one retry, got " .. tostring(state.set_calls))
assert(cache_store["mymaster@10.0.0.11:26379"] == "10.0.0.2:6379",
    "cache should point at the new master after retry")

-- Pipeline path: same demoted-master then retry once.
cache_store["mymaster@10.0.0.11:26379"] = "10.0.0.1:6379"
state.set_calls = 0
state.pipeline_commits = 0
client:init_pipeline()
client:set("k2", "v2")
local results, perr = client:commit_pipeline()
assert(perr == nil, "pipeline retry should succeed, err=" .. tostring(perr))
assert(type(results) == "table" and results[1] == "OK", "pipeline should return OK after retry")
assert(state.pipeline_commits == 2, "expected one failed pipeline + one retry")
assert(cache_store["mymaster@10.0.0.11:26379"] == "10.0.0.2:6379")

-- Pipeline: persistent READONLY on retry must surface err (not silent success).
cache_store["mymaster@10.0.0.11:26379"] = "10.0.0.1:6379"
state.sticky_demoted = true
state.pipeline_commits = 0
client:init_pipeline()
client:set("k3", "v3")
client:set("k4", "v4")
results, perr = client:commit_pipeline()
assert(perr ~= nil and tostring(perr):find("READONLY", 1, true),
    "persistent READONLY after pipeline retry must surface err, got " .. tostring(perr))
assert(state.pipeline_commits == 2, "expected one failed pipeline + one retry")
state.sticky_demoted = false

print("redis_iresty sentinel failover tests OK")
