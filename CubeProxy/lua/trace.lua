-- Attach a CubeProxy span to an existing sampled trace and pass its context
-- to the sandbox. Export uses a bounded worker queue and an OpenResty timer.

local _M = { _VERSION = "0.01" }

local SPAN_KIND_SERVER = 2
local STATUS_UNSET = 0
local STATUS_ERROR = 2

local DEFAULT_SERVICE_NAME = "cube-proxy"
local DEFAULT_FLUSH_INTERVAL_MS = 2000
local DEFAULT_MAX_QUEUE = 4096
local DEFAULT_MAX_BATCH = 512
local DEFAULT_TIMEOUT_MS = 2000
local OTLP_CONTENT_TYPE = "application/json"

-- Worker-local configuration, filled once by setup() from init_worker_by_lua.
local cfg = {
    enabled = false,
    host = nil,
    port = nil,
    path = "/v1/traces",
    service_name = DEFAULT_SERVICE_NAME,
    sample_ratio = 1.0,
    flush_interval_ms = DEFAULT_FLUSH_INTERVAL_MS,
    max_queue = DEFAULT_MAX_QUEUE,
    max_batch = DEFAULT_MAX_BATCH,
    timeout_ms = DEFAULT_TIMEOUT_MS,
}

-- Worker-local batch queue. A timer owns draining it; request phases only
-- append, so no locking is needed (nginx workers are single-threaded and
-- cosocket yields cannot interleave two appends).
local queue = {}
local queue_len = 0
local flushing = false
local dropped_spans = 0
local failed_exports = 0

-- ── small helpers ──────────────────────────────────────────────────────────

local function log_warn(fmt, ...)
    ngx.log(ngx.WARN, "LEVEL_WARN||", string.format("cube_trace: " .. fmt, ...))
end

local function log_error(fmt, ...)
    ngx.log(ngx.ERR, "LEVEL_ERROR||", string.format("cube_trace: " .. fmt, ...))
end

local HEX32 = "^%x%x%x%x%x%x%x%x%x%x%x%x%x%x%x%x" ..
              "%x%x%x%x%x%x%x%x%x%x%x%x%x%x%x%x$"
local HEX16 = "^%x%x%x%x%x%x%x%x%x%x%x%x%x%x%x%x$"
local HEX2 = "^%x%x$"

-- An all-zero trace-id / span-id is the W3C "invalid" sentinel.
local function is_zero_id(id)
    return not id:find("[^0]")
end

-- Span ids must be unpredictable enough not to collide across workers.
local random_bytes
do
    -- Probe the whole OpenSSL-backed path once, at load time, rather than
    -- trusting a successful require(). resty.random binds RAND_pseudo_bytes
    -- through FFI and OpenSSL removed that symbol in 1.1.0, so on the base
    -- image the module loads fine and only raises when bytes() is actually
    -- called — which would put a Lua error on the data-plane request path for
    -- every traced request. Deciding here keeps that off the hot path.
    local function try_resty()
        local resty_random = require "resty.random"
        local resty_string = require "resty.string"
        local probe = resty_random.bytes(8)
        if not probe or not resty_string.to_hex(probe) then
            return nil
        end
        return function(n)
            local bytes = resty_random.bytes(n)
            if not bytes then
                return nil
            end
            return resty_string.to_hex(bytes)
        end
    end

    local ok, fast = pcall(try_resty)
    if ok and type(fast) == "function" then
        random_bytes = fast
    else
        -- init_worker_phase.lua seeds math.random with ngx.now() * 1000 +
        -- worker.id(). Every worker starts inside the same millisecond, so
        -- those seeds differ only by the worker index and the resulting
        -- streams stay correlated. Mix the pid in before falling back to it;
        -- span ids only have to be distinct, not secret.
        local ok_pid, pid = pcall(function() return ngx.worker.pid() end)
        if ok_pid and pid then
            math.randomseed(pid * 2654435761 + ngx.now() * 1000)
        end
        random_bytes = function(n)
            local t = {}
            for i = 1, n * 2 do
                t[i] = string.format("%x", math.random(0, 15))
            end
            return table.concat(t)
        end
    end
end

local function new_span_id()
    local id = random_bytes(8)
    -- Reject the (astronomically unlikely) zero id so we never emit an
    -- invalid span context.
    while id == nil or is_zero_id(id) do
        id = random_bytes(8)
        if id == nil then
            return nil
        end
    end
    return id:lower()
end

-- Unix milliseconds -> OTLP UnixNano. Must be built as a decimal string:
-- a nanosecond epoch (~1.8e18) exceeds the 53-bit mantissa of a Lua number,
-- so formatting it directly would silently corrupt the low digits.
local function unix_nano(ms)
    local sec = math.floor(ms / 1000)
    local nano = math.floor((ms - sec * 1000) * 1000000)
    if nano < 0 then
        nano = 0
    elseif nano > 999999999 then
        nano = 999999999
    end
    return string.format("%d%09d", sec, nano)
end

-- Minimal JSON string encoder. URIs, host headers and error text all land in
-- attributes, so control characters and quotes must be escaped. %c (not the
-- Lua-5.1-only %z) keeps this parseable by the stock `lua` that `make test`
-- may fall back to.
local function json_escape(s)
    s = s:gsub('[%c\\"]', function(c)
        if c == '"' then return '\\"' end
        if c == "\\" then return "\\\\" end
        if c == "\n" then return "\\n" end
        if c == "\r" then return "\\r" end
        if c == "\t" then return "\\t" end
        return string.format("\\u%04x", string.byte(c))
    end)
    return s
end

local function json_string(s)
    return '"' .. json_escape(tostring(s)) .. '"'
end

-- OTLP AnyValue: the type tag must match the JSON shape, so pick one here.
local function otlp_value(v)
    local t = type(v)
    if t == "boolean" then
        return '{"boolValue":' .. (v and "true" or "false") .. '}'
    elseif t == "number" then
        if math.floor(v) == v and math.abs(v) < 2 ^ 53 then
            return '{"intValue":' .. string.format("%d", v) .. '}'
        end
        return '{"doubleValue":' .. string.format("%.9g", v) .. '}'
    end
    return '{"stringValue":' .. json_string(v) .. '}'
end

local function encode_attributes(attrs)
    local parts = {}
    local n = 0
    for _, k in ipairs(attrs.order) do
        local v = attrs[k]
        if v ~= nil then
            n = n + 1
            parts[n] = '{"key":' .. json_string(k) .. ',"value":' .. otlp_value(v) .. '}'
        end
    end
    return "[" .. table.concat(parts, ",") .. "]"
end

-- ── traceparent parsing ──────────────────────────────────────────

-- Parse a W3C traceparent. Returns trace_id, parent_span_id, sampled or nil.
-- Deliberately strict: a malformed header must fall back to "not traced"
-- rather than to a half-parsed context that would produce a broken tree.
local function parse_traceparent(header)
    if header == nil or header == "" then
        return nil
    end
    -- nginx joins duplicate headers with ", "; the leftmost wins.
    local first = header:match("^([^,]+)")
    if not first then
        return nil
    end
    local version, trace_id, parent_id, flags = first:match(
        "^%s*([%x][%x])%-([%x]+)%-([%x]+)%-([%x][%x])%s*$")
    if not version then
        return nil
    end
    -- Version ff is reserved and never valid.
    if version == "ff" then
        return nil
    end
    if not (trace_id:match(HEX32) and parent_id:match(HEX16) and flags:match(HEX2)) then
        return nil
    end
    if is_zero_id(trace_id) or is_zero_id(parent_id) then
        return nil
    end
    return trace_id:lower(), parent_id:lower(), (tonumber(flags, 16) % 2) == 1
end

local function route_name(route_mode)
    if route_mode == "path" then
        return "/sandbox/{sandbox_id}/{container_port}"
    end
    return "/{sandbox_host}"
end

-- ── export ─────────────────────────────────────────────────────────────────

local function encode_batch(batch)
    return '{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":'
        .. otlp_value(cfg.service_name) .. '}]},"scopeSpans":[{"scope":{"name":'
        .. json_string("cubesandbox.cubeproxy.trace") .. '},"spans":['
        .. table.concat(batch, ",") .. "]}]}]}"
end

-- Runs inside ngx.timer: no ngx.var, no ngx.ctx, no request-scoped anything.
local function post_batch(payload)
    local sock = ngx.socket.tcp()
    sock:settimeouts(cfg.timeout_ms, cfg.timeout_ms, cfg.timeout_ms)

    local ok, err = sock:connect(cfg.host, cfg.port)
    if not ok then
        return nil, "connect failed: " .. tostring(err)
    end

    local req = table.concat({
        "POST " .. cfg.path .. " HTTP/1.1",
        "Host: " .. cfg.host .. ":" .. cfg.port,
        "Content-Type: " .. OTLP_CONTENT_TYPE,
        "Content-Length: " .. #payload,
        "Connection: close",
        "",
        "",
    }, "\r\n")

    local sent, send_err = sock:send(req .. payload)
    if not sent then
        sock:close()
        return nil, "send failed: " .. tostring(send_err)
    end

    -- Read the status line only. The collector's body is an empty JSON object
    -- on success and we have no use for it; draining it would just hold the
    -- timer open. Connection: close makes the discard safe.
    local line, read_err = sock:receive("*l")
    sock:close()
    if not line then
        return nil, "receive failed: " .. tostring(read_err)
    end

    local status = tonumber(line:match("HTTP/%d%.%d (%d%d%d)") or "")
    if not status or status < 200 or status >= 300 then
        return nil, "collector returned " .. tostring(line)
    end
    return true
end

local function flush(premature)
    if premature or not cfg.enabled then
        return
    end
    -- A slow collector must not let two flushes race on the same queue.
    if flushing then
        return
    end
    if queue_len == 0 then
        return
    end
    flushing = true

    local batch = queue
    queue = {}
    queue_len = 0

    -- Cap the payload so one burst cannot build an unbounded request body.
    local i = 1
    while i <= #batch do
        local last = math.min(i + cfg.max_batch - 1, #batch)
        local slice = {}
        for j = i, last do
            slice[#slice + 1] = batch[j]
        end
        local ok, result, reason = pcall(post_batch, encode_batch(slice))
        if not ok then
            failed_exports = failed_exports + 1
            log_error("export raised: %s", tostring(result))
        elseif not result then
            failed_exports = failed_exports + 1
            log_warn("export failed (%d span(s) dropped): %s", #slice, tostring(reason))
        end
        i = last + 1
    end

    flushing = false
end

-- ── request-phase API ──────────────────────────────────────────────────────

--[[
    Begin a server span for this request. Called from the rewrite phase, before
    any routing decision, so that rejects are captured too.

    Returns the span table, or nil when tracing is off / the request is not
    part of a traced trace. When nil is returned the request is proxied exactly
    as before: no header is rewritten and nothing is queued.
--]]
function _M.start(route_mode)
    if not cfg.enabled then
        return nil
    end

    local trace_id, parent_id, parent_sampled = parse_traceparent(ngx.var.http_traceparent)
    if not trace_id then
        return nil
    end
    -- parentbased: honour the upstream decision. An unsampled trace stays
    -- unsampled here, and we forward the original header untouched.
    if not parent_sampled then
        return nil
    end
    if cfg.sample_ratio < 1.0 and math.random() >= cfg.sample_ratio then
        return nil
    end

    local span_id = new_span_id()
    if not span_id then
        return nil
    end

    local span = {
        trace_id = trace_id,
        span_id = span_id,
        parent_id = parent_id,
        route_mode = route_mode,
        attrs = { order = {} },
        status_code = STATUS_UNSET,
        status_message = nil,
    }

    local function set(key, value)
        if value == nil then
            return
        end
        if span.attrs[key] == nil then
            span.attrs.order[#span.attrs.order + 1] = key
        end
        span.attrs[key] = value
    end
    span.set = set

    set("cube.route_mode", route_mode)
    set("http.request.method", ngx.var.request_method)
    set("network.protocol.version", ngx.var.server_protocol)

    ngx.ctx.cube_trace = span

    -- Make envd (and anything behind it) our child. Setting the request header
    -- is enough: the data-plane locations do not use proxy_pass_request_headers
    -- off, so nginx forwards it upstream.
    ngx.req.set_header("traceparent",
        string.format("00-%s-%s-01", trace_id, span_id))

    return span
end

function _M.set(key, value)
    if not cfg.enabled then
        return
    end
    local span = ngx.ctx.cube_trace
    if not span then
        return
    end
    span.set(key, value)
end

--[[
    Close the span and queue it for export. Called from log_phase, which must
    stay non-blocking: this only appends to a table and returns.

    ngx.now() is nginx's cached clock, so prefer $request_time (measured by
    nginx itself) for the duration and derive the start from it.
--]]
function _M.finish()
    if not cfg.enabled then
        return
    end
    local span = ngx.ctx.cube_trace
    if not span then
        return
    end
    -- Clear first: log_subrequest is on, and a subrequest that somehow shares
    -- this ctx must not emit the span twice.
    ngx.ctx.cube_trace = nil

    local end_ms = ngx.now() * 1000
    local request_time = tonumber(ngx.var.request_time)
    local start_ms
    if request_time then
        start_ms = end_ms - request_time * 1000
    else
        start_ms = end_ms
    end

    local status = tonumber(ngx.var.status) or 0
    local route = route_name(span.route_mode)
    span.set("http.response.status_code", status)
    span.set("http.route", route)
    if span.status_code == STATUS_UNSET and status >= 500 then
        -- Upstream 5xx is a server fault even when Lua never rejected.
        span.status_code = STATUS_ERROR
        span.status_message = "upstream returned " .. status
    end

    local name = (ngx.var.request_method or "GET") .. " " .. route

    local parts = {
        '{"traceId":' .. json_string(span.trace_id),
        '"spanId":' .. json_string(span.span_id),
        '"parentSpanId":' .. json_string(span.parent_id),
        '"kind":' .. SPAN_KIND_SERVER,
        '"name":' .. json_string(name),
        '"startTimeUnixNano":"' .. unix_nano(start_ms) .. '"',
        '"endTimeUnixNano":"' .. unix_nano(end_ms) .. '"',
        '"attributes":' .. encode_attributes(span.attrs),
    }
    if span.status_code == STATUS_ERROR then
        parts[#parts + 1] = '"status":{"code":' .. STATUS_ERROR .. ',"message":'
            .. json_string(span.status_message or "error") .. '}'
    else
        parts[#parts + 1] = '"status":{"code":' .. STATUS_UNSET .. '}'
    end
    local encoded = table.concat(parts, ",") .. "}"

    if queue_len >= cfg.max_queue then
        dropped_spans = dropped_spans + 1
        -- Rate-limit the complaint: at 10k QPS a full queue would otherwise
        -- produce 10k log lines a second.
        if dropped_spans % 1000 == 1 then
            log_warn("export queue full (%d), dropped %d span(s) so far; "
                .. "collector unreachable or flush interval too long",
                cfg.max_queue, dropped_spans)
        end
        return
    end

    queue_len = queue_len + 1
    queue[queue_len] = encoded

    -- Flush early on a big backlog rather than waiting for the interval timer.
    if queue_len >= cfg.max_batch and not flushing then
        local ok, err = ngx.timer.at(0, flush)
        if not ok then
            log_warn("failed to schedule flush: %s", tostring(err))
        end
    end
end

-- ── worker init ────────────────────────────────────────────────────────────

local function parse_endpoint(endpoint)
    local scheme, host, port, path = endpoint:match("^(%a+)://([^/:]+):?(%d*)(/.*)$")
    if not scheme then
        scheme, host, port, path = endpoint:match("^(%a+)://([^/:]+):?(%d*)$")
        path = "/v1/traces"
    end
    if not scheme or not host then
        return nil
    end
    -- https would need a CA bundle the base image does not ship, and silently
    -- turning off verification is not an option. Reject it loudly instead.
    if scheme ~= "http" then
        return nil, "only http:// endpoints are supported (got " .. scheme .. "://)"
    end
    if not path or path == "" then
        path = "/v1/traces"
    end
    return { host = host, port = tonumber(port) or 4318, path = path }
end

--[[
    Configure tracing for this worker. Called from init_worker_by_lua, which is
    the only phase where os.getenv is reliably available; mirrors
    proxy_registry.setup().

    Recognised keys (all optional):
      enable            boolean, must be true
      endpoint          "http://host:port/v1/traces"
      service_name      defaults to "cube-proxy"
      sample_ratio      0..1, applied on top of the upstream sampled flag
      flush_interval_ms defaults to 2000
      max_queue         defaults to 4096, bounds worker memory
      max_batch         defaults to 512, bounds one request body
      timeout_ms        defaults to 2000, connect/send/read
--]]
function _M.setup(options)
    options = options or {}
    if not options.enable then
        return false
    end

    local endpoint = options.endpoint
    if endpoint == nil or endpoint == "" then
        log_warn("enabled but no endpoint configured; tracing stays off")
        return false
    end

    local parsed, err = parse_endpoint(endpoint)
    if not parsed then
        log_error("invalid endpoint %q: %s; tracing stays off",
            endpoint, tostring(err or "unparsable"))
        return false
    end

    cfg.enabled = true
    cfg.host = parsed.host
    cfg.port = parsed.port
    cfg.path = parsed.path

    if options.service_name and options.service_name ~= "" then
        cfg.service_name = options.service_name
    end
    if options.sample_ratio and options.sample_ratio >= 0 and options.sample_ratio <= 1 then
        cfg.sample_ratio = options.sample_ratio
    end
    if options.flush_interval_ms and options.flush_interval_ms > 0 then
        cfg.flush_interval_ms = options.flush_interval_ms
    end
    if options.max_queue and options.max_queue > 0 then
        cfg.max_queue = options.max_queue
    end
    if options.max_batch and options.max_batch > 0 then
        cfg.max_batch = options.max_batch
    end
    if options.timeout_ms and options.timeout_ms > 0 then
        cfg.timeout_ms = options.timeout_ms
    end

    local ok, timer_err = ngx.timer.every(cfg.flush_interval_ms / 1000, flush)
    if not ok then
        -- Without the interval timer spans would only leave the worker on the
        -- backlog trigger, which is not good enough. Fail closed.
        cfg.enabled = false
        log_error("cannot start flush timer: %s; tracing disabled", tostring(timer_err))
        return false
    end

    ngx.log(ngx.INFO, "cube_trace: exporting to http://", cfg.host, ":", cfg.port, cfg.path,
        " as service ", cfg.service_name)
    return true
end

-- Test/inspection hooks. Not used on the request path.
function _M._config()
    return cfg
end

function _M._stats()
    return { queued = queue_len, dropped = dropped_spans, failed_exports = failed_exports }
end

function _M._drain_for_test()
    local batch = queue
    queue = {}
    queue_len = 0
    return batch
end

return _M
