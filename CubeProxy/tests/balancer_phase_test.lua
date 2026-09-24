-- Unit coverage for balancer_phase.lua's connect retry:
--   * first try (get_last_failure == nil) grants exactly one extra try;
--   * a retry invocation (get_last_failure ~= nil) grants none, so the total
--     is bounded to a single retry;
--   * the resolved peer is set on every invocation;
--   * the set_current_peer failure path exits 503 and logs the formatted
--     diagnostic (guards the string.format arg-order regression);
--   * a failed / clamped set_more_tries is logged but never aborts the request.
-- The file is a phase script (runs on load, no module table), so each case
-- loads it fresh with a mocked ngx / ngx.balancer. The mocks follow the real
-- runtime (OpenResty 1.21.4.1): ngx.log renders nil as "nil", and the
-- balancer return shapes match lua-resty-core's ngx/balancer.lua.

local function run_phase(opts)
    opts = opts or {}
    local calls = { more_tries = 0, set_peer = nil, exit = nil, logs = {} }
    -- Return shapes mirror lua-resty-core 0.1.23 (bundled with OpenResty
    -- 1.21.4.1), ngx/balancer.lua: success is `true`, set_more_tries returns
    -- `true, warning` when it clamps the request (lines 138-142), failure is
    -- `nil, msg`.
    local balancer = {
        get_last_failure = function() return opts.last_failure end,
        set_more_tries = function(n)
            calls.more_tries = calls.more_tries + n
            if opts.more_fail then return nil, "set_more_tries boom" end
            if opts.more_note then return true, "reduced tries due to limit" end
            return true
        end,
        set_current_peer = function(ip, port)
            calls.set_peer = ip .. ":" .. port
            if opts.peer_fail then return nil, "connection refused" end
            return true
        end,
    }
    _G.ngx = {
        ERR = "ERR", WARN = "WARN",
        var = { backend_ip = "10.0.0.5", backend_port = "20042" },
        log = function(...)
            local n = select("#", ...)
            local parts = {}
            for i = 1, n do
                parts[i] = tostring((select(i, ...)))
            end
            table.insert(calls.logs, table.concat(parts, ""))
        end,
        -- Raise like the sibling suites' mock (sandbox_route_lookup_test.lua):
        -- ngx.exit ends the handler at runtime, so the test must not run code
        -- placed after it either.
        exit = function(code) error({ status = code }, 0) end,
    }
    package.loaded["ngx.balancer"] = balancer
    package.loaded["utils"] = { is_null = function(_, v) return v == nil or v == "" end }

    local chunk = assert(loadfile("./lua/balancer_phase.lua"))
    local ok, err = pcall(chunk)
    if not ok then
        if type(err) ~= "table" or err.status == nil then
            error(err, 0)
        end
        calls.exit = err.status
    end
    return calls
end

local function has_log(calls, needle)
    for _, line in ipairs(calls.logs) do
        if line:find(needle, 1, true) then return true end
    end
    return false
end

-- First try: one retry granted, peer set, no exit.
local first = run_phase({ last_failure = nil })
assert(first.more_tries == 1, "first try must grant exactly one retry, got " .. first.more_tries)
assert(first.set_peer == "10.0.0.5:20042", "peer must be set on first try")
assert(first.exit == nil, "first try must not exit")

-- Retry invocation: no further retries (bounded to one), peer still set.
local retry = run_phase({ last_failure = "failed" })
assert(retry.more_tries == 0, "retry must not grant more tries, got " .. retry.more_tries)
assert(retry.set_peer == "10.0.0.5:20042", "peer must be set on retry")

-- set_current_peer failure: exits 503 and logs the formatted diagnostic.
local peerfail = run_phase({ last_failure = nil, peer_fail = true })
assert(peerfail.exit == 503, "peer failure must exit 503, got " .. tostring(peerfail.exit))
assert(has_log(peerfail, "connect to backend (10.0.0.5:20042) err: connection refused"),
    "peer failure must log the formatted diagnostic")

-- set_more_tries failure: logged as an error, request continues (peer still
-- set, no exit). The mock log joins level, prefix and message.
local morefail = run_phase({ last_failure = nil, more_fail = true })
assert(has_log(morefail, "ERRLEVEL_ERROR||set_more_tries failed: set_more_tries boom"),
    "set_more_tries failure must be logged at ERR with LEVEL_ERROR||")
assert(morefail.set_peer == "10.0.0.5:20042", "request must continue after set_more_tries failure")
assert(morefail.exit == nil, "set_more_tries failure must not abort the request")

-- set_more_tries clamp note: surfaced as a WARN, request continues.
local morenote = run_phase({ last_failure = nil, more_note = true })
assert(has_log(morenote, "WARNLEVEL_WARN||set_more_tries: reduced tries due to limit"),
    "set_more_tries clamp note must be logged at WARN with LEVEL_WARN||")
assert(morenote.set_peer == "10.0.0.5:20042", "request must continue after a clamp note")

print("balancer_phase retry tests OK")
