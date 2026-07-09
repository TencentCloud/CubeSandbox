-- proxy_registry.lua
--
-- Publishes this CubeProxy replica to Redis so cube-lifecycle-manager (CLM)
-- can discover it. Schema (owner: this file; consumer: CLM discovery):
--
--   HSET cube:v1:shared:cube_proxy:registry  <proxy_id> <JSON blob>
--   ZADD cube:v1:shared:cube_proxy:heartbeat <unix_ms>  <proxy_id>
--
-- The registry Hash carries the endpoint metadata CLM needs to reach us
-- (admin_url, resume_url, node_ip, ...). The heartbeat Sorted Set is
-- refreshed on every timer tick so CLM can decide who is alive.
--
-- All work runs off `ngx.timer.every`, gated to worker 0 so a multi-worker
-- nginx doesn't spam duplicate writes. Any Redis / JSON error is logged at
-- ERR level and swallowed — the next tick self-heals.

local cjson = require "cjson.safe"

local _M = { _VERSION = "0.01" }

-- Redis keys — kept in sync with cube-lifecycle-manager/internal/discovery.
local REGISTRY_KEY  = "cube:v1:shared:cube_proxy:registry"
local HEARTBEAT_KEY = "cube:v1:shared:cube_proxy:heartbeat"

-- publish is the single-tick worker. It (re)writes the registry entry the
-- first time it runs (or after a Redis outage), and always refreshes the
-- heartbeat score to now_ms. Every network operation is wrapped in pcall
-- so a transient failure never propagates up into ngx.timer.every, which
-- would tear the timer down permanently.
local function publish(premature, cfg, state)
    if premature then
        return
    end

    local redis = require "redis_iresty"
    local red = redis:new({
        redis_ip    = cfg.redis_ip,
        redis_port  = tonumber(cfg.redis_port) or 6379,
        redis_pd    = cfg.redis_pd,
        redis_index = tonumber(cfg.redis_index) or 0,
        timeout     = 1, -- seconds
    })

    local now_ms = math.floor(ngx.now() * 1000)

    -- (Re)publish the registry Hash on the very first successful call, and
    -- also after any error observed on the previous tick — that way a
    -- transient Redis restart never leaves us in a state where the heartbeat
    -- says "alive" but the registry row is gone.
    if not state.registry_pushed then
        local blob = cjson.encode({
            proxy_id   = cfg.proxy_id,
            admin_url  = cfg.admin_url,
            resume_url = cfg.resume_url,
            node_ip    = cfg.node_ip,
            started_at = state.started_at,
            version    = cfg.version,
        })
        if not blob then
            ngx.log(ngx.ERR, "proxy_registry: cjson encode failed")
            return
        end
        local ok, err = red:hset(REGISTRY_KEY, cfg.proxy_id, blob)
        if not ok then
            ngx.log(ngx.ERR, "proxy_registry: hset failed: ", tostring(err))
            return -- leave state.registry_pushed=false so we retry next tick
        end
        state.registry_pushed = true
    end

    local ok, err = red:zadd(HEARTBEAT_KEY, now_ms, cfg.proxy_id)
    if not ok then
        ngx.log(ngx.ERR, "proxy_registry: zadd failed: ", tostring(err))
        -- Consider the registry row stale too; force a re-push on next tick
        -- so we recover cleanly after Redis flushed its state.
        state.registry_pushed = false
        return
    end

    state.last_pushed_ms = now_ms
    -- Publish the last-heartbeat timestamp in a shared dict so the admin
    -- healthz handler can surface it (see admin_phase.lua).
    local ldict = ngx.shared.local_cache
    if ldict then
        ldict:set("cube_proxy_heartbeat_last_pushed_ms", now_ms)
    end
end

-- setup is called from init_worker_by_lua. It's a no-op on all workers
-- except worker 0 (single-writer discipline) and on unconfigured proxies
-- (cfg.enable == false), so leaving the require + call unconditionally in
-- init_worker is safe.
function _M.setup(cfg)
    if not cfg or not cfg.enable then
        return
    end
    if ngx.worker.id() ~= 0 then
        return
    end
    if not cfg.proxy_id or cfg.proxy_id == "" then
        ngx.log(ngx.ERR, "proxy_registry: proxy_id is empty; timer not started")
        return
    end
    if not cfg.redis_ip or cfg.redis_ip == "" then
        ngx.log(ngx.ERR, "proxy_registry: redis_ip is empty; timer not started")
        return
    end

    local interval_s = (tonumber(cfg.interval_ms) or 5000) / 1000
    if interval_s < 1 then
        interval_s = 1
    end

    local state = {
        started_at      = math.floor(ngx.now() * 1000),
        registry_pushed = false,
        last_pushed_ms  = 0,
    }

    -- Cosockets (used by the Redis client) are disabled during
    -- init_worker_by_lua* itself, so schedule an immediate one-shot timer
    -- that fires *after* init_worker has returned. This makes the row
    -- appear in Redis without waiting for the recurring timer's first
    -- tick, while staying inside a context where cosockets are legal.
    local ok0, err0 = ngx.timer.at(0, publish, cfg, state)
    if not ok0 then
        ngx.log(ngx.ERR, "proxy_registry: ngx.timer.at(0) failed: ", tostring(err0))
    end

    local ok, err = ngx.timer.every(interval_s, publish, cfg, state)
    if not ok then
        ngx.log(ngx.ERR, "proxy_registry: ngx.timer.every failed: ", tostring(err))
    else
        ngx.log(ngx.NOTICE, "proxy_registry: heartbeat started for ", cfg.proxy_id,
            " every ", interval_s, "s")
    end
end

return _M
