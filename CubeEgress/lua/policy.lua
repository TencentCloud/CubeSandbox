-- CubeSandbox cube-egress — Policy storage.
--
-- Storage: ngx.shared.policy_store (lua_shared_dict declared in nginx.conf).
--   key   = sandbox source IP, e.g. "169.254.68.6"
--   value = cjson-encoded policy table
--
-- Index: a special key "__index__" holds a cjson array of all known
-- sandbox_ip strings. We maintain it manually rather than relying on
-- shared_dict:get_keys() because the latter caps at 1024 and is
-- O(n_keys_in_dict) — both unacceptable for large fleets.
--
-- Concurrency: lua_shared_dict get/set are atomic and lock-free, but
-- the index update path (read index, modify, write back) is not. The
-- admin API is single-writer in practice (one external system), so
-- a strict CAS loop would be over-engineering. Instead we
-- serialize index mutations with shared_dict:add() on a sentinel key.

local cjson = require "cjson.safe"
-- resty.openssl.digest is already used by cert_signer.lua (sha256 leaf
-- signing), so the runtime cost of pulling it in here is amortized.
local digest_lib = require "resty.openssl.digest"

local _M = {}

local STORE = "policy_store"
local INDEX_KEY = "__index__"
local INDEX_LOCK_KEY = "__index_lock__"
local INDEX_LOCK_TIMEOUT_MS = 1000

-- Inline secret value cap. 64 KiB matches the old standalone secret_store
-- per-entry limit; anything larger is almost certainly a misconfiguration
-- (and would bloat policy_store fast).
local SECRET_MAX_BYTES = 65536

-- ---------- validation ----------

-- IPv4 dotted-quad. No IPv6 for now.
local IPV4_PATTERN = "^([0-9]+)%.([0-9]+)%.([0-9]+)%.([0-9]+)$"

local function is_valid_sandbox_ip(s)
    if type(s) ~= "string" then return false end
    local a, b, c, d = string.match(s, IPV4_PATTERN)
    if not a then return false end
    for _, octet in ipairs({a, b, c, d}) do
        local n = tonumber(octet)
        if not n or n < 0 or n > 255 then return false end
        -- Disallow leading zeros to avoid "010" looking like 8 vs 10.
        if #octet > 1 and string.sub(octet, 1, 1) == "0" then return false end
    end
    return true
end

-- Validate policy structure. Returns (true, nil) or (false, err_string).
-- Strict on required fields; permissive on optional (forward-compat).
local function validate_policy(p)
    if type(p) ~= "table" then return false, "policy must be an object" end
    if type(p.policy_id) ~= "string" or p.policy_id == "" then
        return false, "policy.policy_id required (non-empty string)"
    end
    if type(p.rules) ~= "table" then
        return false, "policy.rules required (array)"
    end
    -- Lua arrays are tables with integer keys 1..N. cjson decodes JSON
    -- arrays as such. We reject objects-shaped-as-rules early.
    local n = #p.rules
    if n == 0 then
        return false, "policy.rules must have at least one rule"
    end
    local seen_ids = {}
    for i = 1, n do
        local r = p.rules[i]
        if type(r) ~= "table" then
            return false, "rules[" .. i .. "] must be an object"
        end
        if type(r.id) ~= "string" or r.id == "" then
            return false, "rules[" .. i .. "].id required (non-empty string)"
        end
        if seen_ids[r.id] then
            return false, "rules[" .. i .. "].id duplicates earlier rule '" .. r.id .. "'"
        end
        seen_ids[r.id] = true
        if type(r.match) ~= "table" then
            return false, "rules[" .. i .. "].match required (object; empty {} allowed)"
        end
        if type(r.action) ~= "table" then
            return false, "rules[" .. i .. "].action required (object)"
        end
        if type(r.action.allow) ~= "boolean" then
            return false, "rules[" .. i .. "].action.allow required (boolean)"
        end
        -- inject is optional; if present must be array of {header, secret, format}.
        if r.action.inject ~= nil then
            if type(r.action.inject) ~= "table" then
                return false, "rules[" .. i .. "].action.inject must be array"
            end
            for j, inj in ipairs(r.action.inject) do
                if type(inj) ~= "table" then
                    return false, string.format("rules[%d].action.inject[%d] must be object", i, j)
                end
                if type(inj.header) ~= "string" or inj.header == "" then
                    return false, string.format("rules[%d].action.inject[%d].header required", i, j)
                end
                if type(inj.secret) ~= "string" or inj.secret == "" then
                    return false, string.format(
                        "rules[%d].action.inject[%d].secret required (non-empty string)", i, j)
                end
                if #inj.secret > SECRET_MAX_BYTES then
                    return false, string.format(
                        "rules[%d].action.inject[%d].secret exceeds %d bytes",
                        i, j, SECRET_MAX_BYTES)
                end
                -- format optional; defaults to "${SECRET}" at inject time
            end
        end
        -- audit optional; default "metadata" at decision time
        if r.action.audit ~= nil and r.action.audit ~= "full" and r.action.audit ~= "metadata" and r.action.audit ~= "none" then
            return false, string.format("rules[%d].action.audit must be full|metadata|none", i)
        end
    end
    return true, nil
end

_M.is_valid_sandbox_ip = is_valid_sandbox_ip
_M.validate_policy     = validate_policy

-- ---------- inline-secret fingerprint ----------
--
-- Inject directives now embed the secret value inline (`inject[].secret`)
-- rather than reference an out-of-band secret_id. To keep the audit
-- schema (`secret_ids: [...]`) and admin redaction logic stable, we
-- synthesize a content-derived ID for each inline secret and stamp it
-- onto the inject entry as `secret_ref_synthetic`.
--
-- Format: "fp-" + first 8 hex chars of sha256(value).
--   - "fp-" prefix distinguishes synthetic refs from any historical
--     operator-chosen secret_id (no collision risk).
--   - 8 hex chars (32 bits) is plenty to disambiguate within a single
--     policy's audit log; this is an opaque debug handle, not a
--     security primitive — DO NOT use it for auth or as a unique key.
--   - Identical secret values produce identical fingerprints. That is
--     intentional: it lets the audit reader correlate uses of the same
--     credential across requests without exposing the credential itself.
--
-- Failure mode: if the digest API fails (very unlikely — same lib that
-- signs every TLS leaf), we fall back to "fp-unknown" and log. The
-- inject still proceeds; the synthetic ref is decorative for audit only.
local function compute_fingerprint(value)
    local d, err = digest_lib.new("sha256")
    if not d then
        ngx.log(ngx.ERR, "fingerprint digest.new failed: ", tostring(err))
        return "fp-unknown"
    end
    local ok, uerr = d:update(value)
    if not ok then
        ngx.log(ngx.ERR, "fingerprint digest.update failed: ", tostring(uerr))
        return "fp-unknown"
    end
    local raw, ferr = d:final()
    if not raw then
        ngx.log(ngx.ERR, "fingerprint digest.final failed: ", tostring(ferr))
        return "fp-unknown"
    end
    -- Take first 4 bytes -> 8 hex chars.
    local b1, b2, b3, b4 = string.byte(raw, 1, 4)
    return string.format("fp-%02x%02x%02x%02x", b1, b2, b3, b4)
end
_M._compute_fingerprint = compute_fingerprint  -- exported for tests

-- ---------- index helpers ----------

local function shared()
    local d = ngx.shared[STORE]
    if not d then
        error("lua_shared_dict '" .. STORE .. "' not declared in nginx.conf")
    end
    return d
end

local function read_index()
    local d = shared()
    local raw = d:get(INDEX_KEY)
    if not raw then return {} end
    local arr = cjson.decode(raw)
    if type(arr) ~= "table" then return {} end
    return arr
end

-- Atomic-ish index update. Single-writer assumption (see file header).
local function with_index_lock(fn)
    local d = shared()
    local got, err = d:add(INDEX_LOCK_KEY, 1, INDEX_LOCK_TIMEOUT_MS / 1000)
    if not got then
        return nil, "index lock contended: " .. (err or "unknown")
    end
    local ok, ret_or_err = pcall(fn)
    d:delete(INDEX_LOCK_KEY)
    if not ok then return nil, "index op failed: " .. tostring(ret_or_err) end
    return ret_or_err, nil
end

local function index_add(sandbox_ip)
    return with_index_lock(function()
        local d = shared()
        local arr = read_index()
        for _, ip in ipairs(arr) do
            if ip == sandbox_ip then return true end
        end
        table.insert(arr, sandbox_ip)
        local ok, err = d:set(INDEX_KEY, cjson.encode(arr))
        if not ok then error("index set: " .. tostring(err)) end
        return true
    end)
end

local function index_remove(sandbox_ip)
    return with_index_lock(function()
        local d = shared()
        local arr = read_index()
        local out = {}
        for _, ip in ipairs(arr) do
            if ip ~= sandbox_ip then table.insert(out, ip) end
        end
        local ok, err = d:set(INDEX_KEY, cjson.encode(out))
        if not ok then error("index set: " .. tostring(err)) end
        return true
    end)
end

-- ---------- public CRUD ----------

-- list_ips() -> table of sandbox_ip strings (no values)
function _M.list_ips()
    return read_index()
end

-- get(sandbox_ip) -> (policy_table | nil, err)
function _M.get(sandbox_ip)
    if not is_valid_sandbox_ip(sandbox_ip) then
        return nil, "invalid sandbox_ip"
    end
    local raw = shared():get(sandbox_ip)
    if not raw then return nil, nil end
    local p = cjson.decode(raw)
    if type(p) ~= "table" then
        return nil, "policy decode failed (corrupt entry)"
    end
    return p, nil
end

-- put(sandbox_ip, policy_table) -> (true | nil, err)
-- Full replace; validates policy first.
--
-- Side effect: after validation succeeds, every inject entry has a
-- `secret_ref_synthetic` field stamped onto it (sha256 fingerprint of
-- the inline secret value). This is computed once here, on the write
-- path, so the data plane (access_phase.apply_injects) never spends
-- digest cycles per request and audit emits stable IDs across requests
-- for the same secret. The mutation is in-place on the caller's table
-- so the encoded payload includes the field.
function _M.put(sandbox_ip, policy)
    if not is_valid_sandbox_ip(sandbox_ip) then
        return nil, "invalid sandbox_ip"
    end
    local ok, err = validate_policy(policy)
    if not ok then return nil, err end
    -- Stamp fingerprints. Validation has already guaranteed shape, so
    -- defensive type-checks here would be dead code.
    for _, r in ipairs(policy.rules) do
        if r.action.inject then
            for _, inj in ipairs(r.action.inject) do
                inj.secret_ref_synthetic = compute_fingerprint(inj.secret)
            end
        end
    end
    local encoded = cjson.encode(policy)
    if not encoded then return nil, "policy encode failed" end
    local set_ok, set_err = shared():set(sandbox_ip, encoded)
    if not set_ok then return nil, "shared_dict set: " .. tostring(set_err) end
    local _, idx_err = index_add(sandbox_ip)
    if idx_err then
        -- Best-effort: data is in store but index is stale. Surface it.
        ngx.log(ngx.ERR, "policy index_add failed for ", sandbox_ip, ": ", idx_err)
    end
    return true, nil
end

-- delete(sandbox_ip) -> (true | nil, err). Idempotent.
function _M.delete(sandbox_ip)
    if not is_valid_sandbox_ip(sandbox_ip) then
        return nil, "invalid sandbox_ip"
    end
    shared():delete(sandbox_ip)
    local _, idx_err = index_remove(sandbox_ip)
    if idx_err then
        ngx.log(ngx.ERR, "policy index_remove failed for ", sandbox_ip, ": ", idx_err)
    end
    return true, nil
end

-- patch(sandbox_ip, rule) -> (true | nil, err)
-- Insert or replace a single rule by its id; preserves rule order
-- (replace in place; new rules append). The whole policy is re-validated.
function _M.patch(sandbox_ip, rule)
    local p, err = _M.get(sandbox_ip)
    if err then return nil, err end
    if not p then return nil, "policy not found" end
    if type(rule) ~= "table" or type(rule.id) ~= "string" then
        return nil, "rule.id required"
    end
    local replaced = false
    for i, r in ipairs(p.rules) do
        if r.id == rule.id then
            p.rules[i] = rule
            replaced = true
            break
        end
    end
    if not replaced then table.insert(p.rules, rule) end
    return _M.put(sandbox_ip, p)
end

-- dump_all() -> table { sandbox_ip = policy_table, ... }
-- Used by GET /admin/v1/policies and /admin/v1/dump.
function _M.dump_all()
    local out = {}
    for _, ip in ipairs(read_index()) do
        local p, err = _M.get(ip)
        if p then
            out[ip] = p
        elseif err then
            ngx.log(ngx.ERR, "dump skip ", ip, ": ", err)
        end
    end
    return out
end

-- count() -> integer (number of policies in index)
function _M.count()
    return #read_index()
end

-- bulk_load(policies_map) -> (n_loaded, errors_table)
-- Used by bootstrap on startup. Best-effort: bad entries are skipped
-- with warnings; good entries are loaded.
function _M.bulk_load(policies_map)
    if type(policies_map) ~= "table" then
        return 0, { "policies must be an object" }
    end
    local n = 0
    local errs = {}
    for ip, p in pairs(policies_map) do
        local ok, err = _M.put(ip, p)
        if ok then
            n = n + 1
        else
            table.insert(errs, ip .. ": " .. (err or "?"))
        end
    end
    return n, errs
end

return _M
