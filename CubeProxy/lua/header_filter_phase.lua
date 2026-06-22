if ngx.status ~= 200 then
    if ngx.var.upstream_addr ~= nil and ngx.var.cube_retcode == "310200" then
        ngx.var.cube_retcode = "330" .. ngx.status
    end
end

ngx.header["X-Cube-Request-Id"] = ngx.var.http_x_cube_request_id
ngx.header["X-Cube-Retcode"] = ngx.var.cube_retcode
