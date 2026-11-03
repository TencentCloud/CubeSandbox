-- Run with resty --shdict 'redis_cluster_slots 1m' in a disposable Cluster.
-- This test performs and reverses one real primary promotion.
local seeds = os.getenv("TEST_REDIS_CLUSTER_NODES")
if not seeds or os.getenv("TEST_REDIS_CLUSTER_FAILOVER") ~= "1" then
    print("SKIP: isolated Cluster failover fixture is not enabled")
    return
end

local redis = require "resty.redis"
local json = require "cjson"
local cluster = require "redis_cluster"
local password = os.getenv("TEST_REDIS_CLUSTER_PASSWORD")
local function connect(address)
    local host, port = address:match("^([^:]+):(%d+)$")
    assert(host, "invalid fixture address")
    local client = redis:new()
    client:set_timeout(5000)
    assert(client:connect(host, tonumber(port)))
    if password and password ~= "" then assert(client:auth(password)) end
    return client
end
local function address(node) return node[1] .. ":" .. node[2] end
local seed = connect(seeds:match("^[^,]+"))
local slots = assert(seed:cluster("SLOTS"))
seed:close()
local target_slot = cluster.slot("127.0.0.1")
local primary_address, replica_address, other_address
for _, row in ipairs(slots) do
    if target_slot >= row[1] and target_slot <= row[2] then
        assert(row[4], "target primary needs a replica")
        primary_address, replica_address = address(row[3]), address(row[4])
    else
        other_address = address(row[3])
    end
end
assert(primary_address and replica_address and other_address)
local primary, replica = connect(primary_address), connect(replica_address)
local timeout_config = assert(primary:config("GET", "cluster-node-timeout"))
local node_timeout = assert(tonumber(timeout_config[2])) / 1000
assert(node_timeout > 0 and node_timeout <= 10, "fixture node timeout outside bounded test budget")
local function wait_for(label, check)
    local deadline = ngx.now() + 15
    repeat
        if check() then return end
        ngx.sleep(0.1)
    until ngx.now() > deadline
    error("timeout: " .. label)
end
local function wait_role(client, expected)
    wait_for(expected, function()
        local role = assert(client:role())
        return role[1] == expected and (expected ~= "slave" or role[4] == "connected")
    end)
end
local function wait_replica(client, endpoint)
    local id = assert(client:cluster("MYID"))
    wait_for("replica topology", function()
        for _, slot_range in ipairs(slots) do
            for index = 3, #slot_range do
                local observer = connect(address(slot_range[index]))
                local rows = observer:cluster("REPLICAS", id)
                observer:close()
                local found = false
                if rows then
                    for _, row in ipairs(rows) do
                        local fields = {}
                        for word in row:gmatch("%S+") do fields[#fields + 1] = word end
                        if fields[2]:match("^[^@]+") == endpoint and fields[8] == "connected" then found = true end
                    end
                end
                if not found then return false end
            end
        end
        return true
    end)
end

-- Observe actual socket calls, preserving their arguments and responses.
local trace = {}
local original_connect, original_migrate = redis.connect, redis.migrate
assert(original_migrate, "resty.redis MIGRATE command is unavailable")
redis.connect = function(self, host, port, ...)
    self.validation_address = host .. ":" .. port
    return original_connect(self, host, port, ...)
end
redis.migrate = function(self, ...)
    local result, err = original_migrate(self, ...)
    trace[#trace + 1] = {address=self.validation_address, result=result, error=err}
    return result, err
end

local opts = {redis_cluster_nodes=other_address .. "," .. replica_address,
    redis_index=0, redis_pd=password, timeout=5000}
local cache_key = opts.redis_cluster_nodes .. ":" .. target_slot
local dict = assert(ngx.shared.redis_cluster_slots)
local promoted, promoted_at = false, nil
local ok, failure = pcall(function()
    -- A real MOVED response fills the cache with the current primary.
    local _, err = cluster.execute(opts, "get", {"127.0.0.1"})
    assert(not err, err)
    assert(dict:get(cache_key) == primary_address, "MOVED did not warm the slot cache")
    assert(replica:cluster("FAILOVER"))
    promoted = true
    promoted_at = ngx.now()
    wait_role(primary, "slave")
    wait_role(replica, "master")
    -- Empty KEYS avoids data changes and the destination socket entirely.
    local value, err = cluster.execute(opts, "migrate", {"127.0.0.1", 1, "", 0, 100, "KEYS"})
    assert(not err and value == "NOKEY", err or tostring(value))
    assert(#trace == 2, "expected exactly two command attempts")
    assert(trace[1].address == primary_address and trace[1].error:match("^READONLY "))
    assert(trace[2].address == replica_address and trace[2].result == "NOKEY" and not trace[2].error)
    assert(dict:get(cache_key) == nil, "stale slot cache was not invalidated")
    print(json.encode({native_readonly=1, successful_retries=1, stale_cache_invalidated=true,
        data_keys_touched=0, trace=trace}))
end)

local restored, restore_error = pcall(function()
    if promoted then
        -- Let Redis expire the previous election state before reversing it.
        local remaining = promoted_at + 4 * node_timeout + 1 - ngx.now()
        if remaining > 0 then ngx.sleep(remaining) end
        wait_role(primary, "slave")
        wait_replica(replica, primary_address)
        assert(primary:cluster("FAILOVER"))
        wait_role(primary, "master")
        wait_role(replica, "slave")
        wait_replica(primary, replica_address)
        print(json.encode({restored_primary=primary_address, restored_replica=replica_address}))
    end
end)
dict:delete(cache_key)
primary:close()
replica:close()
redis.connect, redis.migrate = original_connect, original_migrate
assert(restored, restore_error)
assert(ok, failure)
print("Native Redis Cluster READONLY routing test passed")
