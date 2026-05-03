local utils = require "utils"

-- Parse Host: <container_port>-<sandbox_id>.<domain> e.g. 49983-7c8fbcd45ffe450fb8f7fb223ad45507.cube.app
-- Returns container_port, ins_id (sandbox / instance id), or nil, nil on failure.
local function parse_port_and_instance_from_host(host)
    if utils:is_null(host) then
        return nil, nil
    end
    local hostname = host:match("^([^%:]+)")
    if utils:is_null(hostname) then
        hostname = host
    end
    local container_port, ins_id = hostname:match("^(%d+)%-([^%.]+)")
    if not container_port or not ins_id or ins_id == "" then
        return nil, nil
    end
    return container_port, ins_id
end

local function get_cache_timeout()
    return math.random(tonumber(ngx.var.timeout_min), tonumber(ngx.var.timeout_max))
end

local function get_caller_host_ip()
    if not utils:is_null(ngx.var.cube_proxy_host_ip) then
        return ngx.var.cube_proxy_host_ip
    end
    if not utils:is_null(ngx.var.server_addr) then
        return ngx.var.server_addr
    end
    return ""
end

local function load_sandbox_proxy_metadata(ins_id)
    local redis = require "redis_iresty"
    local red = redis:new({
        redis_ip = ngx.var.redis_ip,
        redis_port = ngx.var.redis_port,
        redis_pd = ngx.var.redis_pd,
        redis_index = ngx.var.redis_index
    })

    local key = "bypass_host_proxy:" .. ins_id
    local value, err
    for i = 1, 3 do
        value, err = red:hgetall(key)
        if not err then
            break
        end
        ngx.log(ngx.ERR, "LEVEL_WARN||",
            string.format("request %s using key %s get redis err: %s, retry %d", ngx.var.http_x_cube_request_id, key, err, i))
    end
    if err then
        return nil, string.format("request %s using key %s get redis err: %s",
            ngx.var.http_x_cube_request_id, key, err)
    end
    if not value then
        return nil, string.format("request %s using key %s get redis nil",
            ngx.var.http_x_cube_request_id, key)
    end
    return value, nil
end

--[[
    Get upstream server address
    2 args:
        - ins_id: string
        - container_port: string, "8080" or "32000"
    2 return values:
        - host_ip: string
        - host_port: string
--]]
local function get_backend_address(ins_id, container_port)
    local caller_host_ip = get_caller_host_ip()
    local cache = ngx.shared.local_cache
    local timeout = get_cache_timeout()
    local cache_backend_ip_key = string.format("%s:%s:%s", ins_id, container_port, "backend_ip")
    local cache_backend_port_key = string.format("%s:%s:%s", ins_id, container_port, "backend_port")
    local host_ip = cache:get(cache_backend_ip_key)
    local host_port = cache:get(cache_backend_port_key)
    if host_ip and host_port then
        return host_ip, host_port
    end

    local metadata, err = load_sandbox_proxy_metadata(ins_id)
    if err then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", err)
        ngx.var.cube_retcode = "310500"
        ngx.exit(500)
    end

    local metadata_map = {}
    for i = 1, #metadata, 2 do
        local k = metadata[i]
        local v = metadata[i + 1]
        metadata_map[k] = v
        cache:set(ins_id .. ":" .. k, v, timeout)
    end

    local target_host_ip = metadata_map["HostIP"]
    local target_sandbox_ip = metadata_map["SandboxIP"]
    if utils:is_null(target_host_ip) then
        ngx.log(ngx.ERR, "LEVEL_WARN||",
            string.format("request %s using instance %s misses HostIP", ngx.var.http_x_cube_request_id, ins_id))
        ngx.var.cube_retcode = "310507"
        ngx.exit(500)
    end

    if not utils:is_null(caller_host_ip) and caller_host_ip == target_host_ip then
        if utils:is_null(target_sandbox_ip) then
            ngx.log(ngx.ERR, "LEVEL_ERROR||",
                string.format("request %s instance %s on local host %s misses SandboxIP",
                    ngx.var.http_x_cube_request_id, ins_id, caller_host_ip))
            ngx.var.cube_retcode = "310507"
            ngx.exit(500)
        end
        host_ip = target_sandbox_ip
        host_port = container_port
    else
        host_ip = target_host_ip
        host_port = metadata_map[container_port]
        if utils:is_null(host_port) then
            ngx.log(ngx.ERR, "LEVEL_ERROR||",
                string.format("request %s instance %s misses host port mapping for container_port %s",
                    ngx.var.http_x_cube_request_id, ins_id, container_port))
            ngx.var.cube_retcode = "310507"
            ngx.exit(500)
        end
    end

    cache:set(cache_backend_ip_key, host_ip, timeout)
    cache:set(cache_backend_port_key, host_port, timeout)
    return host_ip, host_port
end

-- resolve sandbox id and container port from Host: <port>-<sandbox_id>.<domain>
local container_port, ins_id = parse_port_and_instance_from_host(ngx.var.http_host)
if not container_port or not ins_id then
    ngx.log(ngx.ERR, "LEVEL_WARN||",
        string.format("request %s invalid Host for port/instance parse: %s",
            ngx.var.http_x_cube_request_id, ngx.var.http_host))
    ngx.var.cube_retcode = "310400"
    ngx.exit(400)
end

local host_ip, host_port = get_backend_address(ins_id, container_port)
ngx.var.backend_ip = host_ip
ngx.var.backend_port = host_port
ngx.ctx.ins_id = ins_id
ngx.ctx.container_port = container_port

local faulty_backend, err = utils:is_faulty_backend(host_ip, true)
if err then
    ngx.log(ngx.ERR, "LEVEL_ERROR||", "check backend fault err: ", err)
end
if faulty_backend == true then
    ngx.log(ngx.ERR, "LEVEL_ERROR||",
        string.format("request %s use instance %s hits faulty_backend %s", ngx.var.http_x_cube_request_id,
            ins_id, host_ip))
    ngx.var.cube_retcode = "340500"
    ngx.exit(500)
end
