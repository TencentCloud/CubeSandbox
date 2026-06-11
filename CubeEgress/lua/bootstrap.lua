-- CubeSandbox cube-egress — startup state recovery.
--
-- Behavior (worker 0 only, runs from init_worker_by_lua):
--   1. Read CUBE_EGRESS_BOOTSTRAP_URL env. If unset → log WARN, mark
--      bootstrap_status=skipped, return. (Allows local dev / tests
--      without an external admin server; production deployment MUST
--      set it.)
--   2. HTTP GET <url> with timeout + 3 retries (exponential backoff).
--   3. Parse JSON body { "policies": {...} }.  A top-level "secrets"
--      key (legacy) triggers a WARN and is dropped — values must be
--      embedded in the per-rule inject[].secret field instead.
--   4. Bulk-load policies. Mark bootstrap_status=ready.
--   5. On any failure, ngx.log(EMERG) and ngx.exit / os.exit(1):
--      cube-egress MUST NOT serve data-plane traffic without policies.
--
-- The HTTP fetch itself happens in init_worker context, where ngx.timer
-- and cosockets are available. We use lua-resty-http (installed in
-- Dockerfile via luarocks).

local cjson  = require "cjson.safe"
local policy = require "policy"

local _M = {}

local DEFAULT_TIMEOUT_MS = 10 * 1000
local MAX_RETRIES        = 3
local BACKOFF_BASE_MS    = 500

local function set_status(s)
    local meta = ngx.shared.meta_store
    if meta then meta:set("bootstrap_status", s) end
end

-- Worker 0 only — call from init_worker_by_lua.
function _M.run()
    if ngx.worker.id() ~= 0 then return end

    local url = os.getenv("CUBE_EGRESS_BOOTSTRAP_URL")
    if not url or url == "" then
        ngx.log(ngx.WARN, "[bootstrap] CUBE_EGRESS_BOOTSTRAP_URL not set; ",
                          "skipping recovery (dev mode). Production deployments ",
                          "MUST set this env to the external admin /dump endpoint.")
        set_status("skipped")
        return
    end

    set_status("pending")

    -- ngx.timer.at lets the fetch run after init_worker returns; resty.http
    -- can't open cosockets directly inside init_worker.
    local ok, err = ngx.timer.at(0, function(premature)
        if premature then return end
        _M._fetch_and_load(url)
    end)
    if not ok then
        ngx.log(ngx.EMERG, "[bootstrap] timer.at failed: ", err)
        os.exit(1)
    end
end

-- Internal: actually fetch + parse + load. Runs in timer context where
-- cosockets are available.
function _M._fetch_and_load(url)
    local http_ok, http = pcall(require, "resty.http")
    if not http_ok then
        ngx.log(ngx.EMERG, "[bootstrap] lua-resty-http not installed: ", http)
        os.exit(1)
    end

    local last_err
    for attempt = 1, MAX_RETRIES do
        local httpc, err = http.new()
        if not httpc then
            last_err = "http.new: " .. tostring(err)
        else
            httpc:set_timeouts(DEFAULT_TIMEOUT_MS, DEFAULT_TIMEOUT_MS, DEFAULT_TIMEOUT_MS)
            local res, rerr = httpc:request_uri(url, {
                method  = "GET",
                headers = { ["Accept"] = "application/json" },
            })
            if not res then
                last_err = "request_uri: " .. tostring(rerr)
            elseif res.status ~= 200 then
                last_err = "status " .. tostring(res.status) ..
                           ": " .. (res.body or ""):sub(1, 256)
            else
                local obj, derr = cjson.decode(res.body or "")
                if not obj then
                    last_err = "decode: " .. tostring(derr)
                else
                    return _M._apply(obj)
                end
            end
        end
        ngx.log(ngx.WARN, "[bootstrap] attempt ", attempt, "/", MAX_RETRIES, " failed: ", last_err)
        if attempt < MAX_RETRIES then
            ngx.sleep((BACKOFF_BASE_MS * (2 ^ (attempt - 1))) / 1000)
        end
    end

    ngx.log(ngx.EMERG, "[bootstrap] giving up after ", MAX_RETRIES,
                       " attempts; last error: ", last_err)
    set_status("failed")
    os.exit(1)
end

-- Apply parsed dump to stores; on any structural error, fail closed.
function _M._apply(obj)
    if type(obj) ~= "table" then
        ngx.log(ngx.EMERG, "[bootstrap] dump must be object")
        set_status("failed")
        os.exit(1)
    end

    local n_pol, p_errs = policy.bulk_load(obj.policies or {})

    -- A non-empty errs list with zero successful loads in a populated
    -- dump means the schema is broken systemically — fail closed.
    -- Mixed partial success is allowed: surface warnings but keep going.
    if #p_errs > 0 then
        ngx.log(ngx.WARN, "[bootstrap] policy load warnings (", #p_errs, "):")
        for _, e in ipairs(p_errs) do ngx.log(ngx.WARN, "  ", e) end
    end

    if obj.policies and next(obj.policies) and n_pol == 0 then
        ngx.log(ngx.EMERG, "[bootstrap] dump had policies but none loaded; fail-closed")
        set_status("failed")
        os.exit(1)
    end

    set_status("ready")
    ngx.log(ngx.NOTICE, "[bootstrap] loaded ", n_pol, " policies")
end

return _M
