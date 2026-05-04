-- In OpenResty, math.randomseed should be called in the init_worker phase.
-- Without this, all worker processes would start with the same seed (typically 1),
-- causing math.random() to return the same sequence of values across all workers.
-- This is critical for cache TTL jitter and other randomized behaviors to ensure
-- they are truly distributed and don't lead to synchronized stampedes.
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
