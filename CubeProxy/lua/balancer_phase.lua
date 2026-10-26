local utils = require "utils"
if (utils:is_null(ngx.var.backend_ip) or utils:is_null(ngx.var.backend_port)) then
    -- unlikely
    ngx.log(ngx.ERR, "LEVEL_ERROR||", string.format("bad addr (%s:%s)", ngx.var.backend_ip, ngx.var.backend_port))
    ngx.exit(503)
end

local balancer = require "ngx.balancer"
local ok, err = balancer.set_current_peer(ngx.var.backend_ip, ngx.var.backend_port)
if not ok then
    ngx.log(ngx.ERR, "LEVEL_ERROR||", string.format("connect to backend (%s:%s) err: %s"), ngx.var.backend_ip,
        ngx.var.backend_port, err)
    ngx.exit(503)
end
