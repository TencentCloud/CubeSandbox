-- In OpenResty, math.randomseed should be called in the init_worker phase.
-- Without this, all worker processes would start with the same seed (typically 1),
-- causing math.random() to return the same sequence of values across all workers.
-- This is critical for cache TTL jitter and other randomized behaviors to ensure
-- they are truly distributed and don't lead to synchronized stampedes.
math.randomseed(ngx.now() * 1000 + ngx.worker.id())
