-- admin_phase.lua
--
-- Loopback-only admin endpoints used by CubeProxy-sidecar to drive the
-- auto-pause / auto-resume coordination dicts. See nginx.conf admin server
-- block for routing.
--
-- All requests are loopback-bound (the listen directive is 127.0.0.1:8082);
-- additionally, when $cube_admin_token is non-empty, requests must carry
-- a matching X-Cube-Admin-Token header. Mismatch → 403.
--
-- Request bodies are JSON; responses are JSON. Errors carry HTTP status +
-- {"error": "..."} body.

local cjson = require "cjson.safe"

local META  = ngx.shared.cube_sandbox_meta
local STATE = ngx.shared.cube_sandbox_state
local LAST  = ngx.shared.cube_sandbox_last_active

-- ── helpers ────────────────────────────────────────────────────────────────

local function reply(status, body)
    ngx.status = status
    ngx.header["Content-Type"] = "application/json"
    if body ~= nil then
        ngx.print(cjson.encode(body))
    end
    ngx.exit(status)
end

local function reply_error(status, msg)
    reply(status, { error = msg })
end

local function check_token()
    local expected = ngx.var.cube_admin_token
    if not expected or expected == "" then
        return
    end
    local got = ngx.var.http_x_cube_admin_token
    if got ~= expected then
        reply_error(ngx.HTTP_FORBIDDEN, "admin token mismatch")
    end
end

local function read_json_body()
    ngx.req.read_body()
    local raw = ngx.req.get_body_data()
    if not raw or raw == "" then
        return nil, "empty body"
    end
    local obj, err = cjson.decode(raw)
    if not obj then
        return nil, "invalid json: " .. tostring(err)
    end
    if type(obj) ~= "table" then
        return nil, "body must be a JSON object"
    end
    return obj, nil
end

local function require_string(obj, key)
    local v = obj[key]
    if type(v) ~= "string" or v == "" then
        return nil, string.format("field %q must be a non-empty string", key)
    end
    return v, nil
end

-- ── handlers ───────────────────────────────────────────────────────────────

-- POST /admin/meta/upsert
--   body: {"sandbox_id": "...", ...arbitrary metadata...}
--   semantics: stores the JSON-encoded metadata under sandbox_id. Sidecar
--              re-pushes the full snapshot on reconnect, so "set" is enough;
--              we don't merge.
local function handle_meta_upsert()
    local obj, err = read_json_body()
    if not obj then return reply_error(ngx.HTTP_BAD_REQUEST, err) end
    local sid, e2 = require_string(obj, "sandbox_id")
    if not sid then return reply_error(ngx.HTTP_BAD_REQUEST, e2) end

    local payload = cjson.encode(obj)
    local ok, set_err, forcible = META:set(sid, payload)
    if not ok then
        return reply_error(ngx.HTTP_INTERNAL_SERVER_ERROR,
            "meta set failed: " .. tostring(set_err))
    end
    if forcible then
        ngx.log(ngx.WARN, "LEVEL_WARN||",
            "cube_sandbox_meta dict full, an entry was evicted")
    end
    return reply(ngx.HTTP_OK, { ok = true })
end

-- POST /admin/meta/delete
--   body: {"sandbox_id": "..."}
--   semantics: removes meta + state + last_active in one shot, matching the
--              "sandbox is gone" lifecycle event.
local function handle_meta_delete()
    local obj, err = read_json_body()
    if not obj then return reply_error(ngx.HTTP_BAD_REQUEST, err) end
    local sid, e2 = require_string(obj, "sandbox_id")
    if not sid then return reply_error(ngx.HTTP_BAD_REQUEST, e2) end

    META:delete(sid)
    STATE:delete(sid)
    LAST:delete(sid)
    return reply(ngx.HTTP_OK, { ok = true })
end

-- POST /admin/state
--   body: {"sandbox_id": "...", "state": "running|pausing|paused"}
local function handle_state()
    local obj, err = read_json_body()
    if not obj then return reply_error(ngx.HTTP_BAD_REQUEST, err) end
    local sid, e2 = require_string(obj, "sandbox_id")
    if not sid then return reply_error(ngx.HTTP_BAD_REQUEST, e2) end
    local st, e3 = require_string(obj, "state")
    if not st then return reply_error(ngx.HTTP_BAD_REQUEST, e3) end
    if st ~= "running" and st ~= "pausing" and st ~= "paused" then
        return reply_error(ngx.HTTP_BAD_REQUEST,
            "state must be one of running|pausing|paused")
    end

    local ok, set_err, forcible = STATE:set(sid, st)
    if not ok then
        return reply_error(ngx.HTTP_INTERNAL_SERVER_ERROR,
            "state set failed: " .. tostring(set_err))
    end
    if forcible then
        ngx.log(ngx.WARN, "LEVEL_WARN||",
            "cube_sandbox_state dict full, an entry was evicted")
    end
    return reply(ngx.HTTP_OK, { ok = true })
end

-- GET /admin/last_active
-- GET /admin/last_active?since=<unix_ms>
--   Returns {"now": <unix_ms>, "entries": {"<sandbox_id>": <ts_ms>, ...}}.
--   `since` filters: only entries with ts > since are included. Useful for
--   the sidecar's incremental polling loop.
local function handle_last_active()
    local args = ngx.req.get_uri_args(2)
    local since = tonumber(args.since) or 0

    local entries = {}
    -- get_keys(0) returns up to 1024 keys; we iterate until exhausted.
    -- For the dict size we set (8m → ~10w entries) the sidecar should pull
    -- often enough that any single response stays bounded; if not, the
    -- sidecar must use ?since= incrementally.
    local keys = LAST:get_keys(0)
    for _, k in ipairs(keys) do
        local v = LAST:get(k)
        if v and v > since then
            entries[k] = v
        end
    end

    return reply(ngx.HTTP_OK, {
        now     = math.floor(ngx.now() * 1000),
        since   = since,
        count   = #keys,
        entries = entries,
    })
end

local function handle_healthz()
    return reply(ngx.HTTP_OK, {
        ok    = true,
        meta  = META and META:free_space() or nil,
        state = STATE and STATE:free_space() or nil,
        last  = LAST and LAST:free_space() or nil,
    })
end

-- ── dispatch ───────────────────────────────────────────────────────────────

local function dispatch()
    if not META or not STATE or not LAST then
        return reply_error(ngx.HTTP_INTERNAL_SERVER_ERROR,
            "auto-pause shared dicts not configured; check nginx.conf")
    end

    check_token()

    local uri = ngx.var.uri or ""
    local method = ngx.req.get_method()

    if uri == "/admin/healthz" and method == "GET" then
        return handle_healthz()
    elseif uri == "/admin/meta/upsert" and method == "POST" then
        return handle_meta_upsert()
    elseif uri == "/admin/meta/delete" and method == "POST" then
        return handle_meta_delete()
    elseif uri == "/admin/state" and method == "POST" then
        return handle_state()
    elseif uri == "/admin/last_active" and method == "GET" then
        return handle_last_active()
    end

    return reply_error(ngx.HTTP_NOT_FOUND,
        string.format("no route for %s %s", method, uri))
end

dispatch()
