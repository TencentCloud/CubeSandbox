local utils = require "utils"
if (utils:is_null(ngx.var.backend_ip) or utils:is_null(ngx.var.backend_port)) then
    -- unlikely
    ngx.log(ngx.ERR, "LEVEL_ERROR||", string.format("bad addr (%s:%s)", ngx.var.backend_ip, ngx.var.backend_port))
    ngx.exit(503)
end

local balancer = require "ngx.balancer"

-- Grant exactly one retry, and only on the first try. The sandbox host-port
-- forwarding on the node can reset a SYN when a stale session from a recycled
-- sandbox remains on the same (inner_ip, envd_port) tuple; the client then sees
-- "connect() failed (111)" and, without a retry, a 502. The retry opens a new
-- upstream connection from a new source port, which is not bound to the stale
-- session.
--
-- The first-try guard bounds every request to two attempts, and nginx.conf caps
-- both paths again with proxy_next_upstream_tries and grpc_next_upstream_tries
-- (2 each); with those caps set_more_tries(1) is not reduced. ngx.balancer
-- offers no way to scope the granted try to connect errors, so it applies to
-- every *_next_upstream condition, which is `error timeout` on both paths
-- (nginx.conf for proxy_pass, the default for grpc_pass); a 5xx response is not
-- retried. proxy_next_upstream_timeout (5s) limits the proxy_pass retry to
-- failures within the first 5s of a request. Two accepted consequences: a
-- request with an idempotent method (any method other than POST, LOCK and
-- PATCH) is also replayed after an error or timeout within that window while
-- sending the request or reading the response header, and the failed first
-- attempt, at most 5s, adds to that request's latency. Non-idempotent requests
-- (every gRPC call is a POST) are never replayed once sent upstream, because
-- neither *_next_upstream directive lists `non_idempotent`; a connect failure
-- sends nothing, so the RST case is retried for every method. A retried request
-- lists both attempts in $upstream_addr and $upstream_status.
if balancer.get_last_failure() == nil then
    local more_ok, more_err = balancer.set_more_tries(1)
    if not more_ok then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", string.format("set_more_tries failed: %s", tostring(more_err)))
    elseif more_err then
        -- lua-resty-core returns a warning alongside success when the request
        -- is clamped (e.g. "reduced tries due to limit"); the retry still holds.
        ngx.log(ngx.WARN, "LEVEL_WARN||", string.format("set_more_tries: %s", tostring(more_err)))
    end
end

local ok, err = balancer.set_current_peer(ngx.var.backend_ip, ngx.var.backend_port)
if not ok then
    ngx.log(ngx.ERR, "LEVEL_ERROR||",
        string.format("connect to backend (%s:%s) err: %s",
            ngx.var.backend_ip, ngx.var.backend_port, tostring(err)))
    ngx.exit(503)
end
