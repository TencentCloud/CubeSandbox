-- Unit tests for lua/trace.lua, runnable under plain `lua` or `luajit`
-- (see the `test` target in the Makefile). Everything OpenResty-specific is
-- stubbed below; nothing here opens a real socket or needs a running nginx.

package.path = "./lua/?.lua;" .. package.path

-- ── ngx stub ───────────────────────────────────────────────────────────────

local forwarded_headers = {}
local timers = {}
local sent_payloads = {}
local collector_status_line = "HTTP/1.1 200 OK"
local collector_connect_err = nil

-- 1763456789.5 is exactly representable as a double, so the nanosecond fields
-- derived from it are exact and can be asserted byte-for-byte.
local FIXED_NOW = 1763456789.5

ngx = {
    ERR = "ERR",
    WARN = "WARN",
    INFO = "INFO",
    var = {},
    ctx = {},
    req = {
        set_header = function(name, value)
            forwarded_headers[name] = value
        end,
    },
    now = function() return FIXED_NOW end,
    -- Present so trace.lua takes the pid-mixed reseed branch of its
    -- math.random fallback. The fallback is the real path on the production
    -- base image, where resty.random's FFI binding to RAND_pseudo_bytes fails.
    worker = {
        pid = function() return 4242 end,
        id = function() return 3 end,
    },
    log = function() end,
    unescape_uri = function(s)
        return (s:gsub("%%(%x%x)", function(h) return string.char(tonumber(h, 16)) end))
    end,
    socket = {
        tcp = function()
            return {
                settimeouts = function() end,
                connect = function()
                    if collector_connect_err then
                        return nil, collector_connect_err
                    end
                    return true
                end,
                send = function(_, data)
                    sent_payloads[#sent_payloads + 1] = data
                    return #data
                end,
                receive = function() return collector_status_line end,
                close = function() end,
            }
        end,
    },
    timer = {
        at = function(delay, cb)
            timers[#timers + 1] = { delay = delay, cb = cb }
            return true
        end,
        every = function(interval, cb)
            timers[#timers + 1] = { interval = interval, cb = cb }
            return true
        end,
    },
}

local trace = require "trace"

-- ── helpers ────────────────────────────────────────────────────────────────

local function expect(cond, msg)
    assert(cond, msg or "assertion failed")
end

local function expect_eq(got, want, what)
    assert(got == want, string.format("%s: got=%s want=%s",
        what, tostring(got), tostring(want)))
end

-- Starts a clean "request". Only the request-scoped state is reset: the flush
-- timers registered by setup() must survive across requests.
--
-- Note: `vars` is merged with pairs(), so a key set to nil does NOT clear the
-- default below. Clear such a variable explicitly on ngx.var instead.
local function new_request(vars)
    forwarded_headers = {}
    ngx.ctx = {}
    ngx.var = {
        request_method = "GET",
        uri = "/",
        host = "cube.app",
        server_protocol = "HTTP/1.1",
        status = "200",
        request_time = "0.250",
    }
    for k, v in pairs(vars or {}) do
        ngx.var[k] = v
    end
    return forwarded_headers
end

local TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
local PARENT_ID = "00f067aa0ba902b7"
local SAMPLED = "00-" .. TRACE_ID .. "-" .. PARENT_ID .. "-01"

-- Runs one traced request end to end and returns the encoded span JSON plus
-- the live span table.
local function traced_span(vars, route_mode)
    vars = vars or {}
    if vars.http_traceparent == nil then
        vars.http_traceparent = SAMPLED
    end
    new_request(vars)
    local span = trace.start(route_mode or "host")
    expect(span ~= nil, "test expected start() to open a span")
    trace.finish()
    local batch = trace._drain_for_test()
    expect(#batch == 1, "expected exactly one queued span, got " .. #batch)
    return batch[1], span
end

local function span_name(encoded)
    return encoded:match('"name":"([^"]*)"')
end

-- Pulls an attribute back out of the encoded span. Returns nil when absent,
-- which is what makes the "must never appear" assertions below meaningful.
local function attr(encoded, key)
    local k = key:gsub("%.", "%%.")
    local needle = '{"key":"' .. k .. '","value":'
    return encoded:match(needle .. '{"stringValue":"([^"]*)"}}')
        or encoded:match(needle .. '{"intValue":([%d%-]+)}}')
        or encoded:match(needle .. '{"boolValue":(%a+)}}')
end

local function flush_timer()
    for i = #timers, 1, -1 do
        if timers[i].interval then
            return timers[i]
        end
    end
    return nil
end

-- ── inert when not configured ──────────────────────────────────────────────
-- Must run before any successful setup(): once enabled, cfg stays enabled for
-- the life of the worker.
--
-- The phase files call set/finish on every request whether or
-- not tracing is on, so a disabled module must not touch request-scoped API at
-- all. Dropping ngx.ctx makes that observable — it is also what the existing
-- sandbox_backend tests' stubs look like, so this is a real regression guard
-- rather than a hypothetical one.
do
    local saved_ctx = ngx.ctx
    ngx.ctx = nil
    expect(trace.start("host") == nil, "start must be inert when unconfigured")
    expect(trace.set("cube.sandbox_id", "sbx") == nil, "set must be inert when unconfigured")
    expect(trace.finish() == nil, "finish must be inert when unconfigured")
    expect(#trace._drain_for_test() == 0, "an inert module must queue nothing")
    ngx.ctx = saved_ctx
end

-- ── setup(): endpoint validation ───────────────────────────────────────────

expect(trace.setup({ enable = false }) == false, "disabled setup must return false")
expect(trace.setup({ enable = true }) == false, "setup without an endpoint must return false")
expect(trace.setup({ enable = true, endpoint = "" }) == false, "empty endpoint must be rejected")
expect(trace.setup({ enable = true, endpoint = "not-a-url" }) == false, "unparsable endpoint must be rejected")
-- The base image ships no CA bundle, so an https endpoint would either fail at
-- handshake time or require turning verification off. Reject it up front.
expect(trace.setup({ enable = true, endpoint = "https://collector:4318/v1/traces" }) == false,
    "https endpoint must be rejected")
expect(trace._config().enabled == false, "a rejected endpoint must leave tracing off")

expect(trace.setup({ enable = true, endpoint = "http://collector.example:4318" }) == true,
    "bare http endpoint must be accepted")
expect_eq(trace._config().host, "collector.example", "endpoint host")
expect_eq(trace._config().port, 4318, "endpoint port")
expect_eq(trace._config().path, "/v1/traces", "default OTLP traces path")
expect_eq(trace._config().service_name, "cube-proxy", "default service name")

expect(trace.setup({
    enable = true,
    endpoint = "http://collector.internal:4318/custom/traces",
    service_name = "cube-proxy-canary",
}) == true, "explicit path and service name must be accepted")
expect_eq(trace._config().path, "/custom/traces", "explicit OTLP path")
expect_eq(trace._config().service_name, "cube-proxy-canary", "explicit service name")

-- A timer that cannot be started would leave spans stranded in the worker
-- forever, so setup must fail closed rather than half-enable.
do
    local real_every = ngx.timer.every
    ngx.timer.every = function() return nil, "too many pending timers" end
    expect(trace.setup({ enable = true, endpoint = "http://collector:4318/v1/traces" }) == false,
        "setup must fail closed when the flush timer cannot start")
    expect(trace._config().enabled == false, "a failed timer must leave tracing off")
    ngx.timer.every = real_every
end

-- setup() only overrides the keys it is given, so spell out the service name
-- here: the assertions below match the exported envelope literally.
expect(trace.setup({
    enable = true,
    endpoint = "http://127.0.0.1:4318/v1/traces",
    service_name = "cube-proxy",
}) == true, "re-enable for the request-phase tests")

-- ── "no parent, no record" ─────────────────────────────────────────────────

do
    local h = new_request({ uri = "/files" })
    expect(trace.start("host") == nil, "a request with no traceparent must produce no span")
    expect(h["traceparent"] == nil, "an untraced request must not get a traceparent injected")
    expect(ngx.ctx.cube_trace == nil, "an untraced request must not populate ngx.ctx")
    -- set/finish are all called unconditionally from the phase
    -- files, so they must tolerate having no span.
    trace.set("cube.sandbox_id", "sbx")
    trace.finish()
    expect(#trace._drain_for_test() == 0, "an untraced request must queue nothing")
end

do
    local invalid = {
        ["ff-" .. TRACE_ID .. "-" .. PARENT_ID .. "-01"] = "reserved version ff",
        ["00-00000000000000000000000000000000-" .. PARENT_ID .. "-01"] = "all-zero trace id",
        ["00-" .. TRACE_ID .. "-0000000000000000-01"] = "all-zero parent id",
        ["00-" .. TRACE_ID:sub(1, 31) .. "-" .. PARENT_ID .. "-01"] = "short trace id",
        ["00-" .. TRACE_ID .. "0-" .. PARENT_ID .. "-01"] = "long trace id",
        ["00-" .. TRACE_ID .. "-" .. PARENT_ID:sub(1, 15) .. "-01"] = "short parent id",
        ["00-" .. TRACE_ID .. "-" .. PARENT_ID .. "-1"] = "short flags",
        ["00-" .. TRACE_ID .. "-" .. PARENT_ID .. "-zz"] = "non-hex flags",
        ["00-" .. TRACE_ID .. "-" .. PARENT_ID .. "01"] = "missing dash",
        ["00-" .. TRACE_ID .. "-" .. PARENT_ID .. "-01-extra"] = "trailing field",
        ["garbage"] = "not a traceparent at all",
        [""] = "empty header",
    }
    for header, why in pairs(invalid) do
        local h = new_request({ http_traceparent = header })
        expect(trace.start("host") == nil, "must reject traceparent: " .. why)
        expect(h["traceparent"] == nil, "must not inject a header for: " .. why)
    end
end

-- parentbased: an upstream "do not sample" decision must be honoured, and the
-- original header must survive untouched so the rest of the trace stays intact.
do
    local unsampled = "00-" .. TRACE_ID .. "-" .. PARENT_ID .. "-00"
    local h = new_request({ http_traceparent = unsampled })
    expect(trace.start("host") == nil, "an unsampled parent must not be recorded")
    expect(h["traceparent"] == nil, "an unsampled parent's header must not be rewritten")
end

-- nginx joins duplicate headers with ", "; the leftmost must win.
do
    local other = "11111111111111111111111111111111"
    local h = new_request({ http_traceparent = SAMPLED .. ", 00-" .. other .. "-" .. PARENT_ID .. "-01" })
    local span = trace.start("host")
    expect(span ~= nil, "a duplicated traceparent must still parse")
    expect_eq(span.trace_id, TRACE_ID, "leftmost traceparent must win")
    expect(h["traceparent"]:find(other, 1, true) == nil, "the discarded duplicate must not be propagated")
    trace._drain_for_test()
end

-- ── span context and propagation ───────────────────────────────────────────

do
    local h = new_request({ http_traceparent = SAMPLED, request_method = "POST", uri = "/files" })
    local span = trace.start("host")
    expect(span ~= nil, "a sampled traceparent must open a span")
    expect_eq(span.trace_id, TRACE_ID, "trace id must be inherited verbatim")
    expect_eq(span.parent_id, PARENT_ID, "parent id must be inherited verbatim")
    expect(span.span_id ~= PARENT_ID, "cube-proxy must mint its own span id")
    expect(#span.span_id == 16, "span id must be 8 bytes")
    expect_eq(span.span_id, span.span_id:lower(), "span id must be lowercase hex")
    expect(span.span_id:match("^%x+$") ~= nil, "span id must be hex")
    -- This is the assertion the whole envd plan rests on: whatever nginx's
    -- header defaults are, envd sees cube-proxy as its parent.
    expect_eq(h["traceparent"], "00-" .. TRACE_ID .. "-" .. span.span_id .. "-01",
        "outbound traceparent must make the upstream our child")
    expect(span.attrs["cube.route_mode"] == "host", "route mode must be recorded")
    expect(span.attrs["http.request.method"] == "POST", "method must be recorded")

    -- Two spans in the same trace must not collide.
    local seen = {}
    for _ = 1, 64 do
        new_request({ http_traceparent = SAMPLED })
        local s = trace.start("host")
        expect(not seen[s.span_id], "span ids must not repeat within a worker")
        seen[s.span_id] = true
    end
    trace._drain_for_test()
end

-- ── Stable span names without request path data ────────────────────────────

do
    local encoded = traced_span({
        uri = "/private-filename",
        request_uri = "/private-filename?path=secret",
        request_method = "GET",
    })
    expect_eq(span_name(encoded), "GET /{sandbox_host}", "host-mode span name")
    expect_eq(attr(encoded, "http.route"), "/{sandbox_host}", "host-mode route")
    expect(encoded:find("private-filename", 1, true) == nil, "do not export raw paths")
    expect(encoded:find("secret", 1, true) == nil, "do not export query data")
    local path_span = traced_span({ uri = "/sandbox/sbx-1/3000/files" }, "path")
    expect_eq(span_name(path_span), "GET /sandbox/{sandbox_id}/{container_port}", "path-mode span name")
end

-- ── rejects and upstream faults ────────────────────────────────────────────

do
    local encoded, span = traced_span({ status = "404" })
    -- no reject recorded: a 404 from the upstream is not a cube-proxy error
    expect_eq(span.status_code, 0, "a proxied 404 must stay UNSET")
    expect(encoded:find('"status":{"code":0}', 1, true) ~= nil, "UNSET status must be emitted")
    expect_eq(attr(encoded, "http.response.status_code"), "404", "status code attribute")
end

do
    -- 502/503/504 from the upstream are server faults even when Lua never
    -- rejected anything, and must not be silently UNSET in Tempo.
    local encoded, span = traced_span({ status = "502" })
    expect_eq(span.status_code, 2, "an upstream 5xx must be promoted to ERROR")
    expect(encoded:find('"status":{"code":2,"message":"upstream returned 502"}', 1, true) ~= nil,
        "upstream 5xx must carry a status message")
end

-- ── JSON escaping ──────────────────────────────────────────────────────────

do
    new_request({ http_traceparent = SAMPLED })
    local span = trace.start("host")
    trace.set("cube.test", 'quote" back\\slash nl\n tab\t bell\x07 del\x7f end')
    trace.set("cube.int", 42)
    trace.set("cube.neg", -7)
    trace.set("cube.bool", true)
    trace.set("cube.nil_value", nil)
    trace.finish()
    local encoded = trace._drain_for_test()[1]

    expect(encoded:find('\\"', 1, true) ~= nil, "quotes must be escaped")
    expect(encoded:find('\\\\', 1, true) ~= nil, "backslashes must be escaped")
    expect(encoded:find('\\n', 1, true) ~= nil, "newlines must be escaped")
    expect(encoded:find('\\t', 1, true) ~= nil, "tabs must be escaped")
    expect(encoded:find('\\u0007', 1, true) ~= nil, "control characters must be escaped")
    expect(encoded:find('\\u007f', 1, true) ~= nil, "DEL must be escaped")
    expect(encoded:find("\n", 1, true) == nil, "no raw control character may reach the payload")
    expect(encoded:find('"', 1, true) ~= nil, "structural quotes must survive")
    expect_eq(attr(encoded, "cube.int"), "42", "integer attribute")
    expect_eq(attr(encoded, "cube.neg"), "-7", "negative integer attribute")
    expect_eq(attr(encoded, "cube.bool"), "true", "boolean attribute")
    expect(encoded:find("cube.nil_value", 1, true) == nil, "a nil value must not create an attribute")
    expect(encoded:find('{"key":"cube.test","value":{"stringValue":"', 1, true) ~= nil,
        "a string attribute must use stringValue")
    expect(encoded:find('{"key":"cube.int","value":{"intValue":42}', 1, true) ~= nil,
        "an integer attribute must use intValue, not stringValue")
    -- Re-setting a key must not duplicate it in the ordered attribute list.
    new_request({ http_traceparent = SAMPLED })
    trace.start("host")
    trace.set("cube.dup", "one")
    trace.set("cube.dup", "two")
    trace.finish()
    local e2 = trace._drain_for_test()[1]
    local _, count = e2:gsub('"key":"cube%.dup"', "")
    expect_eq(count, 1, "overwriting an attribute must not duplicate the key")
    expect_eq(attr(e2, "cube.dup"), "two", "the last write wins")
end

-- ── timestamps ─────────────────────────────────────────────────────────────

do
    -- A nanosecond epoch (~1.8e18) does not fit in a double's 53-bit mantissa,
    -- so it has to be assembled as a decimal string. These exact 19-digit
    -- assertions are what would fail if it were formatted as a number instead.
    local encoded = traced_span({ request_time = "0.250" })
    expect(encoded:find('"endTimeUnixNano":"1763456789500000000"', 1, true) ~= nil,
        "end time must be an exact 19-digit nanosecond string")
    expect(encoded:find('"startTimeUnixNano":"1763456789250000000"', 1, true) ~= nil,
        "start time must be derived from $request_time")
    expect(encoded:find('"kind":2', 1, true) ~= nil, "cube-proxy spans are SERVER spans")
    expect(encoded:find('"traceId":"' .. TRACE_ID .. '"', 1, true) ~= nil, "trace id in payload")
    expect(encoded:find('"parentSpanId":"' .. PARENT_ID .. '"', 1, true) ~= nil, "parent id in payload")

    -- Sub-millisecond durations must not round to zero.
    local fast = traced_span({ request_time = "0.001" })
    expect(fast:find('"startTimeUnixNano":"1763456789499000000"', 1, true) ~= nil,
        "a 1ms request must keep a 1ms duration")

    -- A missing $request_time must still produce a well-formed span.
    new_request({ http_traceparent = SAMPLED })
    ngx.var.request_time = nil
    trace.start("host")
    trace.finish()
    local no_rt = trace._drain_for_test()[1]
    expect(no_rt:find('"startTimeUnixNano":"1763456789500000000"', 1, true) ~= nil,
        "without $request_time the span collapses to zero duration")
end

-- ── log_subrequest must not double-emit ────────────────────────────────────

do
    new_request({ http_traceparent = SAMPLED })
    trace.start("host")
    trace.finish()
    trace.finish()
    expect(#trace._drain_for_test() == 1, "finish() must be idempotent per request")
end

-- ── export through the stubbed socket ──────────────────────────────────────

do
    sent_payloads = {}
    new_request({ http_traceparent = SAMPLED, uri = "/flush" })
    trace.start("host")
    trace.finish()
    expect_eq(trace._stats().queued, 1, "the span must sit in the worker queue")

    local timer = flush_timer()
    expect(timer ~= nil, "setup must register a repeating flush timer")
    timer.cb(false)

    expect_eq(#sent_payloads, 1, "the flush must POST exactly once")
    local payload = sent_payloads[1]
    expect(payload:find("POST /v1/traces HTTP/1.1\r\n", 1, true) == 1,
        "the request line must target the OTLP traces path")
    expect(payload:find("Content-Type: application/json\r\n", 1, true) ~= nil, "OTLP/HTTP is JSON")
    expect(payload:find("Connection: close\r\n", 1, true) ~= nil,
        "the response body is never read, so the socket must be closed")
    expect(payload:find('{"resourceSpans":[{"resource":{"attributes":'
        .. '[{"key":"service.name","value":{"stringValue":"cube-proxy"}}]},'
        .. '"scopeSpans":[{"scope":{"name":"cubesandbox.cubeproxy.trace"},"spans":[{',
        1, true) ~= nil, "the OTLP envelope must wrap the batch")

    -- A wrong Content-Length makes the collector hang waiting for a body it
    -- will never receive.
    local header, body = payload:match("^(.-\r\n\r\n)(.*)$")
    expect(header ~= nil and body ~= nil, "payload must split into header and body")
    expect(header:find("Content-Length: " .. #body .. "\r\n", 1, true) ~= nil,
        "Content-Length must match the body byte-for-byte")
    expect_eq(trace._stats().queued, 0, "a successful flush must empty the queue")
    expect_eq(trace._stats().failed_exports, 0, "a 200 must not count as a failure")
end

do
    -- A non-2xx response is counted and the spans are dropped. Retrying inside
    -- a worker with no persistence would only grow the queue without bound.
    collector_status_line = "HTTP/1.1 503 Service Unavailable"
    local before = trace._stats().failed_exports
    sent_payloads = {}
    new_request({ http_traceparent = SAMPLED })
    trace.start("host")
    trace.finish()
    flush_timer().cb(false)
    expect_eq(trace._stats().failed_exports, before + 1, "a non-2xx response must be counted")
    expect_eq(#sent_payloads, 1, "the POST must still have been attempted")
    expect_eq(trace._stats().queued, 0, "failed spans must not be re-queued")
    collector_status_line = "HTTP/1.1 200 OK"
end

do
    collector_connect_err = "connection refused"
    local before = trace._stats().failed_exports
    new_request({ http_traceparent = SAMPLED })
    trace.start("host")
    trace.finish()
    flush_timer().cb(false)
    expect_eq(trace._stats().failed_exports, before + 1, "a connect failure must be counted")
    expect_eq(trace._stats().queued, 0, "a dead collector must not wedge the queue")
    collector_connect_err = nil
end

do
    -- An empty queue must not produce a request at all.
    local before = #sent_payloads
    trace._drain_for_test()
    flush_timer().cb(false)
    expect_eq(#sent_payloads, before, "flushing an empty queue must be a no-op")
    -- ngx.timer calls the callback with premature=true on worker shutdown.
    new_request({ http_traceparent = SAMPLED })
    trace.start("host")
    trace.finish()
    flush_timer().cb(true)
    expect_eq(#sent_payloads, before, "a premature timer must not export")
    trace._drain_for_test()
end

-- ── the queue is bounded ───────────────────────────────────────────────────

do
    expect(trace.setup({
        enable = true,
        endpoint = "http://127.0.0.1:4318/v1/traces",
        max_queue = 2,
        max_batch = 8,
    }) == true, "setup with queue bounds")
    trace._drain_for_test()
    local dropped_before = trace._stats().dropped
    for i = 1, 5 do
        new_request({ http_traceparent = SAMPLED, uri = "/q" .. i })
        trace.start("host")
        trace.finish()
    end
    expect_eq(trace._stats().queued, 2, "the queue must stop at max_queue")
    expect_eq(trace._stats().dropped, dropped_before + 3, "overflow spans must be counted as dropped")
end

do
    -- max_batch splits one backlog into several bounded request bodies.
    trace._drain_for_test()
    for i = 1, 8 do
        new_request({ http_traceparent = SAMPLED })
        trace.start("host")
        trace.finish()
    end
    sent_payloads = {}
    flush_timer().cb(false)
    expect_eq(#sent_payloads, 1, "8 spans at max_batch=8 must fit in one POST")
    expect_eq(trace._stats().queued, 0, "the whole backlog must drain")

    expect(trace.setup({
        enable = true,
        endpoint = "http://127.0.0.1:4318/v1/traces",
        max_queue = 4096,
        max_batch = 3,
    }) == true, "setup with a small max_batch")
    for i = 1, 7 do
        new_request({ http_traceparent = SAMPLED })
        trace.start("host")
        trace.finish()
    end
    sent_payloads = {}
    flush_timer().cb(false)
    expect_eq(#sent_payloads, 3, "7 spans at max_batch=3 must split into 3 POSTs")
    expect_eq(trace._stats().queued, 0, "every slice must drain")
end

-- ── local sampling ─────────────────────────────────────────────────────────

do
    expect(trace.setup({
        enable = true,
        endpoint = "http://127.0.0.1:4318/v1/traces",
        sample_ratio = 0,
    }) == true, "setup with sample_ratio=0")
    local h = new_request({ http_traceparent = SAMPLED })
    expect(trace.start("host") == nil, "sample_ratio=0 must record nothing")
    expect(h["traceparent"] == nil,
        "a locally-unsampled request must forward the upstream header untouched")

    expect(trace.setup({
        enable = true,
        endpoint = "http://127.0.0.1:4318/v1/traces",
        sample_ratio = 1,
    }) == true, "setup with sample_ratio=1")
    new_request({ http_traceparent = SAMPLED })
    expect(trace.start("host") ~= nil, "sample_ratio=1 must record a sampled parent")
    trace._drain_for_test()

    -- Out-of-range ratios must be ignored rather than trusted.
    expect(trace.setup({
        enable = true,
        endpoint = "http://127.0.0.1:4318/v1/traces",
        sample_ratio = 42,
    }) == true, "setup with an out-of-range ratio")
    expect_eq(trace._config().sample_ratio, 1, "an invalid sample_ratio must be ignored")
end

print("trace tests passed")
