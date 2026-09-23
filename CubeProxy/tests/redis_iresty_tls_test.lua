-- Unit coverage for the managed-Redis TLS/AUTH wiring in redis_iresty.lua:
--   * ssl_opts() only fires TLS when the flag is truthy (bool or string form),
--     and always fills server_name so certificate SAN checks pass;
--   * connect() receives those opts (no dependency on a socket-level ssl());
--   * auth_mod() skips AUTH on an empty password (managed Redis with no
--     AuthToken rejects a lone AUTH and drops the whole connection).

package.path = "./lua/?.lua;" .. package.path

ngx = {
    null = {},
    WARN = "WARN",
    shared = { redis_sentinel_master = setmetatable({}, { __index = function() return function() end end }) },
    log = function() end,
}

-- Record what connect() is handed so the test can assert the third argument.
local last_connect = {}
local function new_redis_obj()
    local obj = {}
    function obj:set_timeout() end
    function obj:connect(host, port, opts)
        last_connect = { host = host, port = port, opts = opts }
        return true
    end
    function obj:auth(pw)
        last_connect.auth = pw
        return true
    end
    return obj
end

package.loaded["resty.redis"] = { new = new_redis_obj }

local redis_iresty = require "redis_iresty"

-- ssl_opts contract ---------------------------------------------------------
local off = redis_iresty:new({ redis_ip = "r.example", redis_port = 6379, redis_ssl = false })
assert(redis_iresty._ssl_opts(off) == nil, "ssl off must yield no opts")

for _, truthy in ipairs({ true, "true", "1" }) do
    local on = redis_iresty:new({ redis_ip = "r.example", redis_port = 6379, redis_ssl = truthy })
    local opts = redis_iresty._ssl_opts(on)
    assert(opts and opts.ssl == true, "ssl on (" .. tostring(truthy) .. ") must set ssl=true")
    assert(opts.ssl_verify == true, "must verify the certificate chain")
    assert(opts.server_name == "r.example", "server_name defaults to redis_ip for SAN check")
end

-- A caller-supplied server_name (Sentinel path passes the resolved host) wins.
local on = redis_iresty:new({ redis_ip = "r.example", redis_ssl = "1" })
assert(redis_iresty._ssl_opts(on, "master.example").server_name == "master.example")

-- connect() must receive the ssl opts, proving TLS no longer rides on a
-- non-existent socket ssl() method.
local client = redis_iresty:new({ redis_ip = "r.example", redis_port = 6379, redis_ssl = "1" })
local sock = new_redis_obj()
client:connect_mod(sock)
assert(last_connect.host == "r.example" and last_connect.port == 6379)
assert(last_connect.opts and last_connect.opts.ssl == true, "connect must carry ssl opts")

-- auth_mod contract ---------------------------------------------------------
for _, empty in ipairs({ "", false }) do
    local c = redis_iresty:new({ redis_pd = empty or "" })
    if empty == false then c.redis_pd = nil end
    last_connect.auth = "SENTINEL_UNSET"
    assert(c:auth_mod(new_redis_obj()) == true, "empty password must short-circuit AUTH")
    assert(last_connect.auth == "SENTINEL_UNSET", "no AUTH command may be sent when password is empty")
end

local withpw = redis_iresty:new({ redis_pd = "s3cret" })
withpw:auth_mod(new_redis_obj())
assert(last_connect.auth == "s3cret", "a configured password must be sent via AUTH")

print("redis_iresty TLS/AUTH tests OK")
