-- CubeSandbox cube-egress — Admin API router.
--
-- Listening: 127.0.0.1:9090 (loopback only). Wired in nginx.conf in a
-- dedicated server block. NOT reachable by sandboxes; only by the
-- external management system on the same host.
--
-- Endpoints (all under /admin/v1):
--   GET    /policies                  — full dump (sandbox_ip → REDACTED policy)
--   GET    /policies/{sandbox_ip}     — single REDACTED policy
--   PUT    /policies/{sandbox_ip}     — full replace; secrets inline in inject
--   PATCH  /policies/{sandbox_ip}     — upsert one rule by id; secrets inline
--   DELETE /policies/{sandbox_ip}
--   ANY    /secrets...                — 410 Gone (DEVIATION 15: removed)
--   GET    /health                    — liveness + counts + bootstrap status
--   GET    /dump                      — full state (REDACTED policies)
--
-- Redaction:
--   GET-side responses replace every inject[].secret value with the
--   string "***REDACTED***" and strip secret_ref_synthetic. Callers
--   that need to inspect the actual stored values cannot — by design,
--   the admin GET surface is not allowed to be a credential
--   exfiltration channel. Operators must keep the source of truth
--   externally (control plane / config store).

local cjson  = require "cjson.safe"
local policy = require "policy"

local _M = {}

-- ---------- redaction ----------

-- redact_policy returns a deep copy of the policy with every
-- inject[].secret replaced by a redaction marker, and every
-- secret_ref_synthetic stripped. Operates on a copy so the cached
-- shared_dict view is untouched and the data plane keeps the real
-- secret. Returns nil if input is nil (so callers can chain on get()).
--
-- Why deep-copy instead of mutating then unmutating: cjson decodes
-- shared_dict-stored policies into fresh tables on every get(), so the
-- "live" path doesn't share refs with this redacted view; but if a
-- future change ever caches decoded policies we don't want admin GET
-- to corrupt that cache. Cost is one cjson.encode/decode per GET,
-- which is acceptable for an admin path.
local function redact_policy(p)
    if p == nil then return nil end
    local raw = cjson.encode(p)
    if not raw then return nil end
    local copy = cjson.decode(raw)
    if type(copy) ~= "table" or type(copy.rules) ~= "table" then
        return copy
    end
    for _, r in ipairs(copy.rules) do
        if r.action and type(r.action.inject) == "table" then
            for _, inj in ipairs(r.action.inject) do
                if inj.secret ~= nil then
                    inj.secret = "***REDACTED***"
                end
                inj.secret_ref_synthetic = nil
            end
        end
    end
    return copy
end

local function redact_dump(map)
    local out = {}
    for ip, p in pairs(map) do
        out[ip] = redact_policy(p)
    end
    return out
end

-- ---------- response helpers ----------

local function send_json(status, body)
    ngx.status = status
    ngx.header["Content-Type"] = "application/json"
    if body ~= nil then
        local enc, err = cjson.encode(body)
        if not enc then
            ngx.log(ngx.ERR, "admin response encode failed: ", err)
            ngx.status = 500
            ngx.say('{"error":"encode_failed"}')
            return ngx.exit(500)
        end
        ngx.say(enc)
    end
    return ngx.exit(status)
end

local function err_response(status, msg)
    return send_json(status, { error = msg })
end

local function read_json_body()
    ngx.req.read_body()
    local raw = ngx.req.get_body_data()
    if not raw then
        -- Body may have spilled to a temp file (client_body_buffer_size).
        local fname = ngx.req.get_body_file()
        if fname then
            local f, oerr = io.open(fname, "rb")
            if not f then return nil, "read body file: " .. tostring(oerr) end
            raw = f:read("*a")
            f:close()
        end
    end
    if not raw or raw == "" then return nil, "empty body" end
    local obj, derr = cjson.decode(raw)
    if not obj then return nil, "invalid JSON: " .. tostring(derr) end
    return obj, nil
end

-- ---------- handlers ----------

local function h_get_policies()
    return send_json(200, redact_dump(policy.dump_all()))
end

local function h_get_policy(sandbox_ip)
    local p, err = policy.get(sandbox_ip)
    if err then return err_response(400, err) end
    if not p then return err_response(404, "policy not found") end
    return send_json(200, redact_policy(p))
end

local function h_put_policy(sandbox_ip)
    local body, err = read_json_body()
    if not body then return err_response(400, err) end
    local ok, perr = policy.put(sandbox_ip, body)
    if not ok then return err_response(400, perr) end
    return send_json(200, { ok = true })
end

local function h_patch_policy(sandbox_ip)
    local body, err = read_json_body()
    if not body then return err_response(400, err) end
    local ok, perr = policy.patch(sandbox_ip, body)
    if not ok then return err_response(400, perr) end
    return send_json(200, { ok = true })
end

local function h_delete_policy(sandbox_ip)
    local ok, err = policy.delete(sandbox_ip)
    if not ok then return err_response(400, err) end
    return send_json(200, { ok = true })
end

local function h_health()
    local meta = ngx.shared.meta_store
    local bootstrap_status = (meta and meta:get("bootstrap_status")) or "unknown"

    -- Read version info from the build-time metadata file.
    local version_info = {version = "unknown", commit = "unknown", build_time = "unknown"}
    local f, err = io.open("/etc/cube/version.json", "rb")
    if f then
        local raw = f:read("*a")
        f:close()
        local decoded = cjson.decode(raw)
        if decoded then
            version_info = decoded
        end
    end

    return send_json(200, {
        status            = "ok",
        bootstrap_status  = bootstrap_status,
        policy_count      = policy.count(),
        version           = version_info.version,
        commit            = version_info.commit,
        build_time        = version_info.build_time,
    })
end

local function h_dump()
    return send_json(200, {
        policies = redact_dump(policy.dump_all()),
    })
end

-- ---------- router ----------

-- Pattern table: { method, lua-pattern, handler, capturing? }
-- The capturing flag tells the dispatcher whether the pattern has a
-- capture group; when true, the captured string is passed to handler.
local routes = {
    {"GET",    "^/admin/v1/health$",                     h_health,         false},
    {"GET",    "^/admin/v1/dump$",                       h_dump,           false},
    {"GET",    "^/admin/v1/policies$",                   h_get_policies,   false},
    {"GET",    "^/admin/v1/policies/([%d%.]+)$",         h_get_policy,     true},
    {"PUT",    "^/admin/v1/policies/([%d%.]+)$",         h_put_policy,     true},
    {"PATCH",  "^/admin/v1/policies/([%d%.]+)$",         h_patch_policy,   true},
    {"DELETE", "^/admin/v1/policies/([%d%.]+)$",         h_delete_policy,  true},
}

function _M.dispatch()
    local method = ngx.req.get_method()
    local uri    = ngx.var.uri
    -- First pass: find any pattern that matches the URI regardless of method.
    -- This lets us return 405 with the correct Allow header instead of 404
    -- when the path is right but method is wrong.
    local path_match
    for _, r in ipairs(routes) do
        local rmethod, pat, handler, capturing = r[1], r[2], r[3], r[4]
        local cap = string.match(uri, pat)
        if cap ~= nil then
            if rmethod == method then
                if capturing then
                    return handler(cap)
                else
                    return handler()
                end
            end
            -- Track that *some* method matches this path; collect Allow.
            path_match = path_match or {}
            path_match[#path_match + 1] = rmethod
        end
    end
    if path_match then
        ngx.header["Allow"] = table.concat(path_match, ", ")
        return err_response(405, "method not allowed")
    end
    return err_response(404, "not found")
end

return _M
