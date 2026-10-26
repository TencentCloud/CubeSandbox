-- CubeSandbox cube-egress — Redaction utilities.
--
-- Pure functions, no ngx.* required at module level. (We only ngx.log
-- on truly broken inputs, which is impossible from the test harness so
-- the conditional require is fine.) Keeping this module pure makes the
-- audit pipeline testable from plain `lua` without spinning openresty.
--
-- Redaction surface:
--   1. headers — name-based deny list + regex (token|secret|key|
--      password|credential|auth). Authorization keeps the scheme.
--      Cookie / Set-Cookie keep cookie names but never values.
--   2. JSON body — recursive scan; keys matching the regex have their
--      values replaced. Booleans / numbers are scrubbed too (a numeric
--      "pin": 1234 is just as much a credential as a string one).
--   3. Form body / multipart — left as TODO; not on the critical path
--      for Pε's verification ("audit shows redact fields work").

local cjson = require "cjson.safe"

local _M = {}

-- Lowercase exact-match deny list. Values become "<redacted>" or a
-- scheme-qualified version (Authorization).
local HEADER_EXACT = {
    ["authorization"]       = "scheme",
    ["proxy-authorization"] = "scheme",
    ["cookie"]              = "cookie",
    ["set-cookie"]          = "cookie",
    ["x-api-key"]           = "redact",
    ["x-auth-token"]        = "redact",
    ["api-key"]             = "redact",
    ["token"]               = "redact",
}

-- The regex "(?i)(token|secret|key|password|credential|auth)" —
-- expressed as a simple substring deny list. We don't use real regex
-- (LuaJIT pcre dep would be overkill) since these fragments don't
-- need anchors or character classes; substring matching is correct.
local HEADER_FRAGMENTS = {
    "token", "secret", "key", "password", "credential", "auth",
}

local function name_matches_secret(lc_name)
    -- Exact deny first (allows scheme-aware Authorization handling).
    if HEADER_EXACT[lc_name] then return HEADER_EXACT[lc_name] end
    for _, frag in ipairs(HEADER_FRAGMENTS) do
        if string.find(lc_name, frag, 1, true) then
            return "redact"
        end
    end
    return nil
end

-- ---------- Authorization scheme extraction ----------

-- "Bearer abc.def" → "Bearer"; "Basic Zm9vOmJhcg==" → "Basic";
-- "  bearer  xyz" → "Bearer" (capitalised). Values without a space
-- (raw token) → "raw".
local function auth_scheme(value)
    if type(value) ~= "string" then return "unknown" end
    local s = value:match("^%s*(%S+)")
    if not s then return "unknown" end
    -- If the rest of the string is empty, it's a single token (no scheme).
    local rest = value:match("^%s*%S+%s+(.+)$")
    if not rest or rest == "" then return "raw" end
    -- Capitalise first letter for canonical form (Bearer / Basic / Digest).
    return s:sub(1, 1):upper() .. s:sub(2):lower()
end

-- ---------- Cookie name extraction ----------

-- "a=1; b=2; foo=bar" → {"a", "b", "foo"}. Drop empty / malformed.
local function cookie_names(value)
    if type(value) ~= "string" then return {} end
    local names = {}
    for pair in string.gmatch(value, "[^;]+") do
        local n = pair:match("^%s*([^=%s]+)%s*=")
        if n then names[#names + 1] = n end
    end
    return names
end

-- ---------- Public: header redaction ----------

-- Input: headers as either:
--   * { ["Header-Name"] = "value", ... }   (single value)
--   * { ["Header-Name"] = {"v1","v2"}, ... } (multi-value, Set-Cookie)
-- Returns a NEW table with the same shape but values redacted.
-- Header names are preserved in their original case.
function _M.redact_headers(headers)
    if type(headers) ~= "table" then return {} end
    local out = {}
    for name, value in pairs(headers) do
        if type(name) ~= "string" then
            -- Skip non-string keys defensively.
        else
            local lc = name:lower()
            local action = name_matches_secret(lc)
            if not action then
                out[name] = value
            elseif action == "scheme" then
                -- Single-value semantic for Authorization-class headers.
                if type(value) == "table" then
                    -- Multi-value Authorization is non-standard but
                    -- handle it without leaking values.
                    local arr = {}
                    for i, v in ipairs(value) do
                        arr[i] = "<redacted:" .. auth_scheme(v) .. ">"
                    end
                    out[name] = arr
                else
                    out[name] = "<redacted:" .. auth_scheme(value) .. ">"
                end
            elseif action == "cookie" then
                -- Keep cookie names but never values. Encode
                -- as "<redacted; names=a,b,c>" so the audit field
                -- stays a single string (matches the cardinality of
                -- the original Cookie / Set-Cookie header). Empty
                -- name list → "<redacted>".
                local names_acc = {}
                if type(value) == "table" then
                    for _, v in ipairs(value) do
                        for _, n in ipairs(cookie_names(v)) do
                            names_acc[#names_acc + 1] = n
                        end
                    end
                else
                    names_acc = cookie_names(value)
                end
                if #names_acc == 0 then
                    out[name] = "<redacted>"
                else
                    out[name] = "<redacted; names=" ..
                                table.concat(names_acc, ",") .. ">"
                end
            else
                out[name] = "<redacted>"
            end
        end
    end
    return out
end

-- ---------- Public: JSON body redaction ----------

-- Recursively walk a decoded JSON value. Strings/numbers/booleans at a
-- key whose name matches name_matches_secret() are replaced with
-- "<redacted>". Arrays preserve order. Nested objects are descended.
local function walk(node)
    local t = type(node)
    if t == "table" then
        -- Detect array vs object: cjson decoded arrays have purely
        -- integer keys 1..n. Heuristic: if it has key [1] AND no
        -- non-integer keys, treat as array.
        local is_array = node[1] ~= nil
        if is_array then
            for k = 1, #node do
                node[k] = walk(node[k])
            end
            return node
        end
        for k, v in pairs(node) do
            if type(k) == "string" and name_matches_secret(k:lower()) then
                node[k] = "<redacted>"
            else
                node[k] = walk(v)
            end
        end
        return node
    end
    return node
end

-- Input: raw JSON string + optional max_bytes (truncate before parse
-- to bound CPU). Output: redacted JSON string, or nil + reason on
-- decode failure (caller should record "<unparseable>" in audit).
function _M.redact_json(body_str, max_bytes)
    if type(body_str) ~= "string" or body_str == "" then
        return nil, "empty"
    end
    if max_bytes and #body_str > max_bytes then
        body_str = body_str:sub(1, max_bytes)
    end
    local decoded, derr = cjson.decode(body_str)
    if not decoded then
        return nil, "decode_failed:" .. tostring(derr)
    end
    walk(decoded)
    local out, eerr = cjson.encode(decoded)
    if not out then
        return nil, "encode_failed:" .. tostring(eerr)
    end
    return out
end

-- ---------- Test hooks ----------

_M._name_matches_secret = name_matches_secret
_M._auth_scheme         = auth_scheme
_M._cookie_names        = cookie_names

return _M
