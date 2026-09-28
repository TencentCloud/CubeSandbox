local redis_c = require "resty.redis"

local unpack = unpack or table.unpack

local ok, new_tab = pcall(require, "table.new")
if not ok or type(new_tab) ~= "function" then
    new_tab = function(narr, nrec)
        return {}
    end
end

local _M = new_tab(0, 155)
_M.version = '0.01'

local commands = {"append", "auth", "bgrewriteaof", "bgsave", "bitcount", "bitop", "blpop", "brpop", "brpoplpush",
                  "client", "config", "dbsize", "debug", "decr", "decrby", "del", "discard", "dump", "echo", "eval",
                  "exec", "exists", "expire", "expireat", "flushall", "flushdb", "get", "getbit", "getrange", "getset",
                  "hdel", "hexists", "hget", "hgetall", "hincrby", "hincrbyfloat", "hkeys", "hlen", "hmget", "hmset",
                  "hscan", "hset", "hsetnx", "hvals", "incr", "incrby", "incrbyfloat", "info", "keys", "lastsave",
                  "lindex", "linsert", "llen", "lpop", "lpush", "lpushx", "lrange", "lrem", "lset", "ltrim", "mget",
                  "migrate", "monitor", "move", "mset", "msetnx", "multi", "object", "persist", "pexpire", "pexpireat",
                  "ping", "psetex", "psubscribe", "pttl", "publish", --[[ "punsubscribe", ]] "pubsub", "quit",
                  "randomkey", "rename", "renamenx", "restore", "rpop", "rpoplpush", "rpush", "rpushx", "sadd", "save",
                  "scan", "scard", "script", "sdiff", "sdiffstore", "select", "set", "setbit", "setex", "setnx",
                  "setrange", "shutdown", "sinter", "sinterstore", "sismember", "slaveof", "slowlog", "smembers",
                  "smove", "sort", "spop", "srandmember", "srem", "sscan", "strlen", --[[ "subscribe",  ]] "sunion",
                  "sunionstore", "sync", "time", "ttl", "type", --[[ "unsubscribe", ]] "unwatch", "watch", "zadd",
                  "zcard", "zcount", "zincrby", "zinterstore", "zrange", "zrangebyscore", "zrank", "zrem",
                  "zremrangebyrank", "zremrangebyscore", "zrevrange", "zrevrangebyscore", "zrevrank", "zscan", "zscore",
                  "zunionstore", "evalsha"}

local mt = {
    __index = _M
}

local function trim(s)
    return (s:gsub("^%s*(.-)%s*$", "%1"))
end

local function parse_sentinel_nodes(raw)
    local out = {}
    if not raw or raw == "" then
        return out
    end
    for part in string.gmatch(raw, "[^,]+") do
        part = trim(part)
        if part ~= "" then
            table.insert(out, part)
        end
    end
    return out
end

-- Parse host:port for Sentinel endpoints. Supports:
--   hostname / IPv4 (optional :port, default_port)
--   bracketed IPv6: [2001:db8::1]:26379 or [2001:db8::1]
-- Unbracketed IPv6 with an embedded port is ambiguous and unsupported.
local function split_host_port(addr, default_port)
    local host, port = addr:match("^%[([^%]]+)%]:(%d+)$")
    if host then
        return host, tonumber(port)
    end
    host = addr:match("^%[([^%]]+)%]$")
    if host then
        return host, default_port
    end
    host, port = addr:match("^([^:]+):(%d+)$")
    if host then
        return host, tonumber(port)
    end
    return addr, default_port
end

-- Short TTL for the cached Sentinel master address. Kept small so a failover
-- is observed within seconds even if the stale master stays reachable.
local SENTINEL_MASTER_TTL = 15

local function sentinel_mode(self)
    return self.redis_master_name and self.redis_master_name ~= ""
end

local function sentinel_master_cache_key(self)
    return (self.redis_master_name or "") .. "@" .. (self.redis_sentinel_nodes or "")
end

local function get_cached_master(self)
    local dict = ngx.shared.redis_sentinel_master
    if not dict then
        return nil
    end
    local val = dict:get(sentinel_master_cache_key(self))
    if not val then
        return nil
    end
    -- Master IP from Sentinel may be IPv6 (no brackets); take the trailing :port.
    local ip, port = val:match("^(.+):(%d+)$")
    if ip then
        return ip, tonumber(port)
    end
    return nil
end

local function set_cached_master(self, ip, port)
    local dict = ngx.shared.redis_sentinel_master
    if dict then
        dict:set(sentinel_master_cache_key(self), ip .. ":" .. port, SENTINEL_MASTER_TTL)
    end
end

local function clear_cached_master(self)
    local dict = ngx.shared.redis_sentinel_master
    if dict then
        dict:delete(sentinel_master_cache_key(self))
    end
end

-- Only READONLY is safe to auto-retry: the demoted replica rejected the write,
-- so nothing was applied. Connection/closed/broken errors are ambiguous (the
-- server may have applied the command before the socket died), so those are
-- surfaced to the caller instead of being replayed.
local function is_failover_err(err)
    if err == nil then
        return false
    end
    return tostring(err):find("READONLY", 1, true) ~= nil
end

-- Return the 1-based index of the first resty.redis error reply that is
-- READONLY. Only {false, err} tuples are errors; a plain string value is a
-- successful reply (even if the text happens to contain "READONLY").
local function first_readonly_index(results, err)
    if is_failover_err(err) then
        return 1
    end
    if type(results) ~= "table" then
        return nil
    end
    for i = 1, #results do
        local value = results[i]
        if type(value) == "table" and value[1] == false and is_failover_err(value[2]) then
            return i
        end
    end
    return nil
end

-- 每次连接前通过 Sentinel 查询当前 master, 适配 failover.
local function resolve_master_via_sentinel(self)
    local sentinels = parse_sentinel_nodes(self.redis_sentinel_nodes)
    if #sentinels == 0 then
        return nil, nil, "redis_sentinel_nodes is empty"
    end

    -- Do not fall back to redis_pd: many deployments only set requirepass on
    -- the Redis master, while Sentinel has no AUTH configured.
    local sentinel_pd = self.redis_sentinel_pd

    local last_err
    for _, sentinel_addr in ipairs(sentinels) do
        local host, port = split_host_port(sentinel_addr, 26379)
        local redis = redis_c:new()
        redis:set_timeout(self.timeout)
        local ok, err = redis:connect(host, port)
        if not ok then
            last_err = err
        else
            -- LuaJIT (OpenResty) has no goto; nest auth so AUTH failure can
            -- skip the SENTINEL probe and try the next sentinel.
            local auth_ok = true
            if sentinel_pd and sentinel_pd ~= "" then
                local auth_err
                auth_ok, auth_err = redis:auth(sentinel_pd)
                if not auth_ok then
                    redis:close()
                    last_err = auth_err
                end
            end
            if auth_ok then
                local res, qerr = redis:sentinel("get-master-addr-by-name", self.redis_master_name)
                redis:close()
                if res and type(res) == "table" and res[1] and res[2] then
                    return res[1], tonumber(res[2])
                end
                last_err = qerr or "bad sentinel reply"
            end
        end
    end
    return nil, nil, last_err or "sentinel lookup failed"
end

local function is_redis_null(res)
    if type(res) == "table" then
        for k, v in pairs(res) do
            if v ~= ngx.null then
                return false
            end
        end
        return true
    elseif res == ngx.null then
        return true
    elseif res == nil then
        return true
    end

    return false
end

function _M.connect_mod(self, redis)
    redis:set_timeout(self.timeout)
    if sentinel_mode(self) then
        -- Fast path: reuse the cached master address to avoid a Sentinel probe
        -- on every command. On a failed connect (likely a failover) drop the
        -- cache entry and fall through to a fresh Sentinel lookup.
        local cip, cport = get_cached_master(self)
        if cip then
            local ok = redis:connect(cip, cport)
            if ok then
                return ok
            end
            clear_cached_master(self)
        end
        local ip, port, err = resolve_master_via_sentinel(self)
        if not ip then
            return nil, err
        end
        -- Cache only after connect succeeds so a transient failure does not
        -- leave a stale address that forces an extra failed connect cycle.
        local ok, cerr = redis:connect(ip, port)
        if not ok then
            return nil, cerr
        end
        set_cached_master(self, ip, port)
        return ok
    end
    return redis:connect(self.redis_ip, self.redis_port)
end

function _M.auth_mod(self, redis)
    return redis:auth(self.redis_pd)
end

function _M.select_mod(self, redis)
    return redis:select(self.redis_index)
end

function _M.set_keepalive_mod(redis)
    -- put it into the connection pool of size 100, with 60 seconds max idle time
    return redis:set_keepalive(60000, 1000)
end

function _M.init_pipeline(self)
    self._reqs = {}
end

local function exec_pipeline(self, reqs)
    local redis, err = redis_c:new()
    if not redis then
        return nil, err
    end

    local ok, cerr = self:connect_mod(redis)
    if not ok then
        return {}, cerr
    end

    ok, err = self:auth_mod(redis)
    if not ok or err then
        pcall(function() redis:close() end)
        return {}, err
    end

    ok, err = self:select_mod(redis)
    if not ok or err then
        pcall(function() redis:close() end)
        return {}, err
    end

    redis:init_pipeline()
    for _, vals in ipairs(reqs) do
        local fun = redis[vals[1]]
        -- Copy args so a failover retry can replay the same commands.
        local args = {unpack(vals, 2)}
        fun(redis, unpack(args))
    end

    local results, perr = redis:commit_pipeline()
    if not results or perr then
        pcall(function() redis:close() end)
        return {}, perr
    end

    -- Inspect READONLY before converting ngx.null→nil (nil holes break ipairs).
    local readonly_at = first_readonly_index(results, perr)

    if is_redis_null(results) then
        results = {}
        ngx.log(ngx.WARN, "is null")
    end

    self.set_keepalive_mod(redis)

    for i = 1, #results do
        if is_redis_null(results[i]) then
            results[i] = nil
        end
    end

    return results, perr, readonly_at
end

function _M.commit_pipeline(self)
    local reqs = self._reqs

    if nil == reqs or 0 == #reqs then
        return {}, "no pipeline"
    else
        self._reqs = nil
    end

    local results, err, readonly_at = exec_pipeline(self, reqs)
    if sentinel_mode(self) and readonly_at then
        -- Replay only the suffix starting at the first READONLY reply. Earlier
        -- commands already applied on the old master must not be re-sent.
        clear_cached_master(self)
        local suffix = {}
        for i = readonly_at, #reqs do
            suffix[#suffix + 1] = reqs[i]
        end
        local retry_results, retry_err = exec_pipeline(self, suffix)
        if retry_err then
            return results, retry_err
        end
        -- resty.redis puts per-command failures in the results table as
        -- {false, err} with a nil top-level err. If the retry is still
        -- READONLY, do not merge and claim success — match do_command.
        local retry_ro = first_readonly_index(retry_results, retry_err)
        if retry_ro then
            local value = retry_results[retry_ro]
            local msg = retry_err
            if type(value) == "table" then
                msg = value[2] or msg
            end
            return results, msg or "READONLY"
        end
        -- Numeric length: retry_results may contain nil holes (ngx.null→nil)
        -- and ipairs would stop early, leaving stale READONLY tuples in place.
        for i = 1, #suffix do
            results[readonly_at + i - 1] = retry_results[i]
        end
        err = retry_err
    end
    return results, err
end

local function exec_command(self, cmd, ...)
    local redis, err = redis_c:new()
    if not redis then
        return nil, err
    end

    local ok, cerr = self:connect_mod(redis)
    if not ok or cerr then
        return nil, cerr
    end

    ok, err = self:auth_mod(redis)
    if not ok or err then
        pcall(function() redis:close() end)
        return nil, err
    end

    ok, err = self:select_mod(redis)
    if not ok or err then
        pcall(function() redis:close() end)
        return nil, err
    end

    local fun = redis[cmd]
    local result, rerr = fun(redis, ...)
    if not result or rerr then
        pcall(function() redis:close() end)
        return nil, rerr
    end

    if is_redis_null(result) then
        result = nil
    end

    self.set_keepalive_mod(redis)
    return result, rerr
end

local function do_command(self, cmd, ...)
    if self._reqs then
        table.insert(self._reqs, {cmd, ...})
        return
    end

    local result, err = exec_command(self, cmd, ...)
    -- At most one retry after Sentinel failover signals. READONLY means the
    -- write did not apply on the demoted replica, so retrying is safe.
    if (not result or err) and sentinel_mode(self) and is_failover_err(err) then
        clear_cached_master(self)
        result, err = exec_command(self, cmd, ...)
    end
    if not result or err then
        return nil, err
    end
    return result, err
end

for i = 1, #commands do
    local cmd = commands[i]
    _M[cmd] = function(self, ...)
        return do_command(self, cmd, ...)
    end
end

function _M.new(self, opts)
    opts = opts or {}
    local timeout = (opts.timeout and opts.timeout * 1000) or 1000

    return setmetatable({
        timeout = timeout,
        redis_index = opts.redis_index or 0,
        redis_ip = opts.redis_ip or "",
        redis_port = opts.redis_port or 0,
        redis_pd = opts.redis_pd or "",
        redis_master_name = opts.redis_master_name or "",
        redis_sentinel_nodes = opts.redis_sentinel_nodes or "",
        redis_sentinel_pd = opts.redis_sentinel_pd or "",
        _reqs = nil
    }, mt)
end

-- Exported for unit tests only.
_M._split_host_port = split_host_port
_M._is_failover_err = is_failover_err

return _M
