-- Native Redis Cluster routing. Only explicit server rejections are retried;
-- transport errors are ambiguous and must never replay an applied write.
local redis_c = require "resty.redis"
local bit = require "bit"
local unpack = unpack or table.unpack
local M = {}

local function split_endpoint(addr)
    local host, port = addr:match("^%[([^%]]+)%]:(%d+)$")
    if not host then host, port = addr:match("^([^:]+):(%d+)$") end
    if host and tonumber(port) and tonumber(port) > 0 and tonumber(port) <= 65535 then
        return host, tonumber(port)
    end
end

local function seeds(raw)
    local out = {}
    for addr in (raw or ""):gmatch("[^,]+") do
        addr = addr:match("^%s*(.-)%s*$")
        if addr ~= "" then
            if not split_endpoint(addr) then return nil, "invalid Redis Cluster seed endpoint" end
            out[#out + 1] = addr
        end
    end
    if #out == 0 then return nil, "Redis Cluster requires seed nodes" end
    return out
end

local function slot(key)
    key = tostring(key)
    local tag = key:match("{([^}]*)}")
    if tag and tag ~= "" then key = tag end
    local crc = 0
    for i = 1, #key do
        crc = bit.bxor(crc, bit.lshift(key:byte(i), 8))
        for _ = 1, 8 do
            if bit.band(crc, 0x8000) ~= 0 then
                crc = bit.bxor(bit.lshift(crc, 1), 0x1021)
            else
                crc = bit.lshift(crc, 1)
            end
            crc = bit.band(crc, 0xffff)
        end
    end
    return bit.band(crc, 0x3fff)
end

local function connect(opts, addr)
    local host, port = split_endpoint(addr)
    if not host then return nil, "invalid Redis Cluster redirect endpoint" end
    local redis, err = redis_c:new()
    if not redis then return nil, err end
    redis:set_timeout(opts.timeout)
    local ok
    ok, err = redis:connect(host, port)
    if not ok then redis:close(); return nil, err end
    if opts.redis_pd and opts.redis_pd ~= "" then
        ok, err = redis:auth(opts.redis_pd)
        if not ok then redis:close(); return nil, err end
    end
    return redis
end

function M.execute(opts, cmd, args)
    if tonumber(opts.redis_index or 0) ~= 0 then return nil, "Redis Cluster supports DB 0 only" end
    if opts.redis_master_name and opts.redis_master_name ~= "" then return nil, "Redis Cluster cannot use Sentinel" end
    local nodes, err = seeds(opts.redis_cluster_nodes)
    if not nodes then return nil, err end
    cmd = cmd:lower()
    local key = args[1]
    if cmd == "eval" or cmd == "evalsha" then
        local count = tonumber(args[2])
        if not count or count < 0 or count ~= math.floor(count) or #args < 2 + count then return nil, "invalid script key count" end
        key = count > 0 and args[3] or nil
        for i = 4, 2 + count do
            if slot(args[i]) ~= slot(key) then return nil, "CROSSSLOT script keys must share a hash slot" end
        end
    elseif cmd == "del" or cmd == "unlink" or cmd == "exists" or cmd == "mget" then
        if #args > 1 then
            local total, values = 0, {}
            for i, item in ipairs(args) do
                local value, e = M.execute(opts, cmd, {item})
                if e then return nil, e end
                if cmd == "mget" then values[i] = value[1] else total = total + value end
            end
            return cmd == "mget" and values or total
        end
    elseif cmd == "multi" or cmd == "exec" or cmd == "watch" or cmd == "unwatch" or cmd == "discard"
        or cmd == "mset" or cmd == "msetnx" or cmd == "scan" or cmd == "keys" or cmd == "select" then
        return nil, cmd .. " requires a cluster-aware call site"
    end
    local dict = ngx.shared.redis_cluster_slots
    local cache_key = opts.redis_cluster_nodes .. ":" .. tostring(key and slot(key) or -1)
    local addr = dict and dict:get(cache_key) or nil
    local asking = false
    for attempt = 1, 8 do
        if not addr then addr = nodes[(attempt - 1) % #nodes + 1] end
        local redis, e = connect(opts, addr)
        if not redis then
            -- Connection establishment failed before any business command.
            if dict then dict:delete(cache_key) end
            addr, asking, err = nil, false, e
        else
            if asking then
                local ok
                ok, e = redis:asking()
                if not ok then redis:close(); return nil, e end
            end
            local value
            value, e = redis[cmd](redis, unpack(args))
            if not e then
                redis:set_keepalive(60000, 1000)
                return value
            end
            redis:close()
            local kind, _slot, target = tostring(e):match("^(%u+) (%d+) (.+)$")
            if kind == "MOVED" or kind == "ASK" then
                if not split_endpoint(target) then return nil, "invalid Redis Cluster redirect endpoint" end
                addr, asking = target, kind == "ASK"
                if kind == "MOVED" and dict then dict:set(cache_key, target, 30) end
            elseif tostring(e) == "READONLY" or tostring(e):match("^READONLY ") then
                -- The demoted primary rejected the write before applying it.
                if dict then dict:delete(cache_key) end
                addr, asking = nil, false
            elseif tostring(e):match("^TRYAGAIN") then
                if ngx.sleep then ngx.sleep(0.025) end
            else
                return nil, e
            end
            err = e
        end
    end
    return nil, err or "Redis Cluster redirects exhausted"
end

M.slot = slot
return M
