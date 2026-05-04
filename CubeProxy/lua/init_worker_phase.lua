-- Seed the random number generator for each worker to ensure
-- that cache TTL jitter (math.random) works correctly.
math.randomseed(ngx.now() * 1000 + ngx.worker.id())

local function monitor_cache_usage()
    local cache_free_space = ngx.shared.local_cache:free_space()
    ngx.shared.local_cache:set("cache_free_space", cache_free_space)
end

local worker_id = ngx.worker.id()
-- Only worker 0 performs these timed tasks
-- Even if worker PID is changed, worker ID still keep same
if worker_id == 0 then
    -- Creating the initial timer
    ngx.timer.every(60, monitor_cache_usage)
end
