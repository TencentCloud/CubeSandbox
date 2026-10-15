package.path = "./lua/?.lua;" .. package.path

local function assert_eq(actual, expected, msg)
    if actual ~= expected then
        error((msg or "assert_eq failed") .. ": expected=" .. tostring(expected) .. " actual=" .. tostring(actual), 2)
    end
end

local FakeDict = {}
FakeDict.__index = FakeDict

function FakeDict:new()
    return setmetatable({ data = {} }, self)
end

function FakeDict:incr(key, value, init)
    if self.data[key] == nil then
        self.data[key] = init or 0
    end
    self.data[key] = self.data[key] + value
    return self.data[key]
end

function FakeDict:get(key)
    return self.data[key]
end

function FakeDict:get_keys()
    local keys = {}
    for k, _ in pairs(self.data) do
        table.insert(keys, k)
    end
    table.sort(keys)
    return keys
end

_G.ngx = {
    var = {},
    shared = { cube_proxy_metrics = FakeDict:new() },
    now = function() return 1000.0 end,
    HTTP_OK = 200,
    status = 200,
    header = {},
    print = function(_) end,
    exit = function(_) end,
    log = function(...) end,
    WARN = "WARN",
}

local metrics = require "metrics"

assert_eq(metrics.route_from_uri("/process.Process/Start"), "sandbox_exec", "process start route")
assert_eq(metrics.route_from_uri("/process.Process/Connect"), "sandbox_exec", "process connect route")
assert_eq(metrics.route_from_uri("/sandbox/sb-1/49983/process.Process/Start"), "sandbox_exec", "path process route")
assert_eq(metrics.route_from_uri("/files"), "sandbox_files", "files route")
assert_eq(metrics.route_from_uri("/sandbox/sb-1/49983/files"), "sandbox_files", "path files route")
assert_eq(metrics.route_from_uri("/unknown"), nil, "unknown route skipped")
assert_eq(metrics.normalize_method("GET"), "GET", "GET method")
assert_eq(metrics.normalize_method("POST"), "POST", "POST method")
assert_eq(metrics.normalize_method("DELETE"), nil, "unsupported method skipped")
assert_eq(metrics.result_from_status(200), "success", "200 success")
assert_eq(metrics.result_from_status(302), "success", "302 success")
assert_eq(metrics.result_from_status(404), "fail", "404 fail")
assert_eq(metrics.result_from_status(503), "fail", "503 fail")

ngx.var.uri = "/process.Process/Start"
ngx.var.request_method = "POST"
ngx.status = 200
ngx.var.request_time = "0.120"
metrics.start_current_request()
metrics.finish_current_request()

ngx.var.uri = "/files"
ngx.var.request_method = "GET"
ngx.status = 503
ngx.var.request_time = "0.900"
metrics.start_current_request()

local in_flight_body = metrics.render_prometheus()
assert(in_flight_body:find('cube_proxy_inflight_requests{route="sandbox_files"} 1', 1, true), in_flight_body)

metrics.finish_current_request()

ngx.var.uri = "/untracked"
ngx.var.request_method = "POST"
ngx.status = 200
ngx.var.request_time = "0.010"
metrics.start_current_request()
metrics.finish_current_request()

ngx.var.cube_proxy_metrics_route = ""
ngx.var.uri = "/files"
ngx.var.request_method = "GET"
ngx.status = 400
ngx.var.request_time = "0.000"
metrics.finish_current_request(400)

local rejected_before_start_body = metrics.render_prometheus()
assert(rejected_before_start_body:find(
    'cube_proxy_inflight_requests{route="sandbox_files"} 0',
    1,
    true
), rejected_before_start_body)
assert(rejected_before_start_body:find(
    'cube_proxy_request_total{method="GET",result="fail",route="sandbox_files"} 1',
    1,
    true
), rejected_before_start_body)
assert(not rejected_before_start_body:find(
    'cube_proxy_request_duration_seconds_count{method="GET",result="fail",route="sandbox_files"} 2',
    1,
    true
), rejected_before_start_body)
assert(rejected_before_start_body:find(
    'cube_proxy_request_duration_seconds_sum{method="GET",result="fail",route="sandbox_files"} 0.9',
    1,
    true
), rejected_before_start_body)

ngx.var.uri = "/files"
ngx.var.request_method = "DELETE"
ngx.status = 200
ngx.var.request_time = "0.010"
metrics.start_current_request()
local unsupported_method_body = metrics.render_prometheus()
assert(unsupported_method_body:find('cube_proxy_inflight_requests{route="sandbox_files"} 1', 1, true), unsupported_method_body)
metrics.finish_current_request()

local body = metrics.render_prometheus()
assert(body:find('cube_proxy_request_total{method="POST",result="success",route="sandbox_exec"} 1', 1, true), body)
assert(body:find('cube_proxy_request_total{method="GET",result="fail",route="sandbox_files"} 1', 1, true), body)
assert(body:find('cube_proxy_inflight_requests{route="sandbox_exec"} 0', 1, true), body)
assert(body:find('cube_proxy_inflight_requests{route="sandbox_files"} 0', 1, true), body)
assert(body:find('cube_proxy_request_duration_seconds_bucket{le="0.128",method="POST",result="success",route="sandbox_exec"} 1', 1, true), body)
assert(body:find('cube_proxy_request_duration_seconds_sum{method="POST",result="success",route="sandbox_exec"} 0.12', 1, true), body)
assert(body:find('cube_proxy_request_duration_seconds_count{method="GET",result="fail",route="sandbox_files"} 1', 1, true), body)
assert(not body:find('method="DELETE"', 1, true), body)
assert(body:find("# HELP cube_proxy_request_total", 1, true), body)
assert(body:find("# TYPE cube_proxy_request_total counter", 1, true), body)
assert(body:find("# HELP cube_proxy_inflight_requests", 1, true), body)
assert(body:find("# TYPE cube_proxy_inflight_requests gauge", 1, true), body)
assert(body:find("# HELP cube_proxy_request_duration_seconds", 1, true), body)
assert(body:find("# TYPE cube_proxy_request_duration_seconds histogram", 1, true), body)

print("metrics_test.lua PASS")
