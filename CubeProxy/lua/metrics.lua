-- metrics.lua — CubeProxy request metrics in Prometheus text format.
-- The module is a no-op unless CUBE_PROXY_METRICS_ENABLE is truthy.

local _M = { _VERSION = "0.01" }

-- Upper bounds (seconds) of the cube_proxy_upstream_response_seconds histogram
-- buckets: powers of two from 1ms (2^0 ms) to 8.192s (2^13 ms). Doubling keeps
-- relative resolution constant from fast exec/file calls to slow backend
-- responses with only 14 finite buckets per label set; anything slower is
-- counted only in the +Inf bucket.
local BUCKETS = {
    0.001, 0.002, 0.004, 0.008,
    0.016, 0.032, 0.064, 0.128,
    0.256, 0.512, 1.024, 2.048,
    4.096, 8.192,
}

local function escape_label_value(v)
    v = tostring(v or "")
    v = v:gsub("\\", "\\\\")
    v = v:gsub("\n", "\\n")
    v = v:gsub('"', '\\"')
    return v
end

local function metric_key(kind, route, method, result, suffix)
    if kind == "inflight" then
        return table.concat({ kind, route }, "|")
    end
    if kind == "bucket" then
        return table.concat({ kind, route, method, result, suffix }, "|")
    end
    return table.concat({ kind, route, method, result }, "|")
end

local function parse_key(key)
    local parts = {}
    for part in string.gmatch(key, "[^|]+") do
        table.insert(parts, part)
    end
    return parts[1], parts[2], parts[3], parts[4], parts[5]
end

local function labels(route, method, result, extra)
    local base = string.format('method="%s",result="%s",route="%s"',
        escape_label_value(method),
        escape_label_value(result),
        escape_label_value(route))
    if extra then
        return extra .. "," .. base
    end
    return base
end

-- Path-style URIs (/sandbox/<id>/<port>/...) are reduced to the part after the
-- port, so sandbox IDs and ports never become label values.
function _M.route_from_uri(uri)
    uri = uri or ""
    local path_route = uri:match("^/sandbox/[%w_%-]+/%d+(/.*)$")
    if path_route and path_route ~= "" then
        uri = path_route
    end
    if uri:match("^/process%.Process/") then
        return "sandbox_exec"
    end
    if uri == "/files" then
        return "sandbox_files"
    end
    return nil
end

function _M.normalize_method(method)
    if method == "GET" or method == "POST" then
        return method
    end
    return nil
end

-- $upstream_header_time has one value per upstream attempt, joined by ", "
-- (retries) or " : " (internal redirects); "-" means no response headers.
-- Returns their sum, or nil if no attempt got headers from a sandbox.
function _M.upstream_seconds(value)
    if value == nil or value == "" then
        return nil
    end
    local total, seen = 0, false
    for part in tostring(value):gmatch("[%d%.]+") do
        local n = tonumber(part)
        if n then
            total = total + n
            seen = true
        end
    end
    if not seen then
        return nil
    end
    return total
end

function _M.result_from_status(status)
    status = tonumber(status) or 0
    if status >= 200 and status < 400 then
        return "success"
    end
    return "fail"
end

local enabled_cache

local function is_truthy(v)
    v = tostring(v or ""):lower()
    return v == "1" or v == "on" or v == "true" or v == "yes"
end

-- Opt-in (truthy: 1/on/true/yes). Keep in sync with metrics_enabled() in
-- start.sh. nginx.conf must whitelist the variable with `env` for workers to
-- see it.
function _M.enabled()
    if enabled_cache == nil then
        enabled_cache = is_truthy(os.getenv("CUBE_PROXY_METRICS_ENABLE"))
    end
    return enabled_cache
end

function _M._reset_enabled_cache()
    enabled_cache = nil
end

local function dict()
    return ngx.shared.cube_proxy_metrics
end

local function incr(key, delta)
    local d = dict()
    if not d then
        if ngx and ngx.log then
            ngx.log(ngx.WARN, "cube_proxy_metrics shared dict is not configured")
        end
        return
    end
    local ok, err = d:incr(key, delta, 0)
    if not ok and ngx and ngx.log then
        ngx.log(ngx.WARN, "cube_proxy_metrics incr failed: ", tostring(err))
    end
end

function _M.start(route)
    if not route then
        return
    end
    incr(metric_key("inflight", route), 1)
end

-- upstream_seconds is nil for requests that never reached a sandbox; those are
-- counted in the request total but kept out of the histogram.
function _M.finish(route, method, result, upstream_seconds)
    if not route then
        return
    end

    incr(metric_key("inflight", route), -1)

    if not method or not result then
        return
    end
    incr(metric_key("total", route, method, result), 1)

    upstream_seconds = tonumber(upstream_seconds)
    if not upstream_seconds or upstream_seconds < 0 then
        return
    end

    incr(metric_key("sum", route, method, result), upstream_seconds)
    incr(metric_key("count", route, method, result), 1)

    for _, bucket in ipairs(BUCKETS) do
        if upstream_seconds <= bucket then
            incr(metric_key("bucket", route, method, result, tostring(bucket)), 1)
        end
    end
    incr(metric_key("bucket", route, method, result, "+Inf"), 1)
end

function _M.start_current_request()
    if not _M.enabled() then
        return false
    end
    local route = _M.route_from_uri(ngx.var.uri)
    ngx.var.cube_proxy_metrics_route = route or ""
    if not route then
        return false
    end
    _M.start(route)
    return true, route
end

function _M.finish_current_request(status_override)
    if not _M.enabled() then
        return
    end
    if ngx.ctx and ngx.ctx.cube_proxy_metrics_finished then
        return
    end

    local route = ngx.var.cube_proxy_metrics_route
    if not route or route == "" then
        return
    end
    local method = _M.normalize_method(ngx.var.request_method)
    local status = status_override or ngx.status or ngx.var.status or 0
    local result = _M.result_from_status(status)
    -- Time to first byte from the sandbox rather than total request time, so
    -- long-running commands and large file transfers do not read as service
    -- latency.
    local upstream_seconds = _M.upstream_seconds(ngx.var.upstream_header_time)
    _M.finish(route, method, result, upstream_seconds)

    if ngx.ctx then
        ngx.ctx.cube_proxy_metrics_finished = true
    end
end

function _M.render_prometheus()
    local d = dict()
    local out = {}
    table.insert(out, "# HELP cube_proxy_request_total CubeProxy request total by route, method and result")
    table.insert(out, "# TYPE cube_proxy_request_total counter")
    table.insert(out, "# HELP cube_proxy_inflight_requests CubeProxy in-flight requests by route")
    table.insert(out, "# TYPE cube_proxy_inflight_requests gauge")

    if not d then
        return table.concat(out, "\n") .. "\n"
    end

    local keys = d:get_keys(0)
    table.sort(keys)

    for _, key in ipairs(keys) do
        local kind, route, method, result = parse_key(key)
        local value = d:get(key) or 0
        if kind == "total" then
            table.insert(out, string.format('cube_proxy_request_total{%s} %s',
                labels(route, method, result), tostring(value)))
        elseif kind == "inflight" then
            table.insert(out, string.format('cube_proxy_inflight_requests{route="%s"} %s',
                escape_label_value(route), tostring(value)))
        end
    end

    table.insert(out, "# HELP cube_proxy_upstream_response_seconds Time in seconds until the sandbox starts responding, by route, method and result")
    table.insert(out, "# TYPE cube_proxy_upstream_response_seconds histogram")

    for _, key in ipairs(keys) do
        local kind, route, method, result, suffix = parse_key(key)
        local value = d:get(key) or 0
        if kind == "bucket" then
            table.insert(out, string.format('cube_proxy_upstream_response_seconds_bucket{%s} %s',
                labels(route, method, result, 'le="' .. escape_label_value(suffix) .. '"'),
                tostring(value)))
        end
    end
    for _, key in ipairs(keys) do
        local kind, route, method, result = parse_key(key)
        local value = d:get(key) or 0
        if kind == "sum" then
            table.insert(out, string.format('cube_proxy_upstream_response_seconds_sum{%s} %s',
                labels(route, method, result), tostring(value)))
        elseif kind == "count" then
            table.insert(out, string.format('cube_proxy_upstream_response_seconds_count{%s} %s',
                labels(route, method, result), tostring(value)))
        end
    end

    return table.concat(out, "\n") .. "\n"
end

return _M
