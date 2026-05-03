if ngx.status ~= 200 and ngx.var.upstream_addr and ngx.var.cube_retcode == "310200" then
    local utils = require "utils"
    local ok, err = utils:is_faulty_backend(ngx.var.backend_ip, false)
    if err then
        ngx.log(ngx.ERR, "LEVEL_WARN||", "check backend fault err: ", err)
    end

    local prefix = ok and "340" or "330"
    ngx.var.cube_retcode = prefix .. ngx.status

    if ngx.status == 502 or ngx.status == 504 then
        local bytes_str = ngx.var.upstream_bytes_received
        local upstream_status = ngx.var.upstream_status
        local has_received_data = false
        
        -- Distinguish network failure (0 bytes) from business 5xx (bytes > 0).
        -- As Nginx provides no direct flag, we use upstream_bytes_received to identify failures.
        -- upstream_status is logged for observability but not used in the decision logic.
        if bytes_str then
            for v in string.gmatch(bytes_str, "%d+") do
                if tonumber(v) > 0 then
                    has_received_data = true
                    break
                end
            end
        end

        if not has_received_data then
            -- Network-level Failure (e.g. connection refused). Clear cache.
            local ins_id = ngx.ctx.ins_id
            local port = ngx.ctx.container_port
            if ins_id and port then
                local cache = ngx.shared.local_cache
                local base_key = string.format("%s:%s:", ins_id, port)
                cache:delete(base_key .. "backend_ip")
                cache:delete(base_key .. "backend_port")
                
                ngx.log(ngx.WARN, "LEVEL_WARN||", string.format("Network Failure (5xx). Cache Cleared. Status: %d, UpstreamStatus: %s, Bytes: %s", 
                    ngx.status, tostring(upstream_status), tostring(bytes_str)))
            end
        else
            -- Application-level 5xx. Keep cache.
            ngx.log(ngx.INFO, "LEVEL_INFO||", string.format("App-level 5xx. Cache Kept. Status: %d, UpstreamStatus: %s", 
                ngx.status, tostring(upstream_status)))
        end
    end
end

ngx.header["X-Cube-Request-Id"] = ngx.var.http_x_cube_request_id
ngx.header["X-Cube-Retcode"] = ngx.var.cube_retcode
