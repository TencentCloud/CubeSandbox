-- In OpenResty, math.randomseed should be called in the init_worker phase.
-- Without this, all worker processes would start with the same seed (typically 1),
-- causing math.random() to return the same sequence of values across all workers.
-- This is critical for cache TTL jitter and other randomized behaviors to ensure
-- they are truly distributed and don't lead to synchronized stampedes.
math.randomseed(ngx.now() * 1000 + ngx.worker.id())

-- Register this CubeProxy replica in Redis so cube-lifecycle-manager can
-- discover us. Config comes from environment variables so the operator can
-- flip the feature on without editing nginx.conf (ngx.var.* is unavailable
-- in init_worker_by_lua). All settings are optional; if CUBE_PROXY_REGISTRY_ENABLE
-- is unset the setup call short-circuits.
local proxy_registry = require "proxy_registry"
proxy_registry.setup({
    enable      = (os.getenv("CUBE_PROXY_REGISTRY_ENABLE") == "1"),
    proxy_id    = os.getenv("CUBE_PROXY_ID"),
    admin_url   = os.getenv("CUBE_PROXY_ADMIN_URL"),
    resume_url  = os.getenv("CUBE_PROXY_RESUME_URL"),
    node_ip     = os.getenv("CUBE_PROXY_NODE_IP"),
    version     = os.getenv("CUBE_PROXY_VERSION"),
    interval_ms = tonumber(os.getenv("CUBE_PROXY_HEARTBEAT_INTERVAL_MS") or "") or 5000,
    redis_ip    = os.getenv("CUBE_PROXY_REGISTRY_REDIS_HOST"),
    redis_port  = tonumber(os.getenv("CUBE_PROXY_REGISTRY_REDIS_PORT") or "") or 6379,
    redis_pd    = os.getenv("CUBE_PROXY_REGISTRY_REDIS_PASSWORD") or "",
    redis_index = tonumber(os.getenv("CUBE_PROXY_REGISTRY_REDIS_DB") or "") or 0,
})
