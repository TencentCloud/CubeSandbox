-- CubeSandbox cube-egress — Per-request structured audit log.
--
-- Record format:
--   { ts, request_id, event, sandbox{}, conn{}, tls{}, http{},
--     policy{}, credentials{}, security, redacted_request_body,
--     redacted_response_body }
--
-- Compatibility note: stdout still gets the main_json format
-- (driven by nginx's own access_log directive in nginx.conf, which
-- this module does NOT touch). Only the FILE_PATH line evolves to the
-- new schema. This is intentional: the stdout line is for ops grep;
-- the file is the compliance record. Splitting the schemas avoids
-- breaking any current log parser consuming /dev/stdout.
--
-- Double-write strategy:
--   - stdout: via nginx access_log directive (we do nothing).
--   - file:   open-append-close per request to FILE_PATH.
--     O_APPEND writes < PIPE_BUF (4096B on Linux) are atomic.
--     Most records are well under that. Records over the
--     PIPE_BUF limit (large redacted_request_body) are still atomic
--     on ext4/xfs because nginx workers don't share fd's between
--     processes here (we open per-request); concurrent multi-record
--     writes from the SAME worker are inherently serialised.

local cjson    = require "cjson.safe"
local redactor = require "redactor"

local _M = {}

local FILE_PATH

-- ---------- bootstrap / init_worker ----------

function _M.bootstrap(opts)
    FILE_PATH = opts.file_path or "/data/log/cube-egress/access.jsonl"
end

function _M.init_worker()
    local f, err = io.open(FILE_PATH, "a")
    if not f then
        ngx.log(ngx.EMERG, "audit log not writable: ", FILE_PATH, ": ", err)
        os.exit(1)
    end
    f:close()
end

-- ---------- record assembly ----------

-- Compute the security.reason field for a decision. Used by BOTH the
-- merged http_request line (build_record below) AND the dedicated
-- security_event line (write_security_event), so the two stay in sync.
--
-- Priority:
--   1. explicit reason argument (caller knows something we don't, e.g.
--      audit.write_one's upstream-failure detector passing
--      "upstream_unreachable_or_unverified").
--   2. d.inject_dropped — set by access_phase when an inject gate
--      failed (e.g. "host_sni_mismatch" for G4).
--   3. d.reason — set by deny() (e.g. "no_policy_for_sandbox").
--   4. First entry's reason from d.injected_skipped — set by
--      apply_injects when a secret_ref is missing
--      (e.g. "secret_not_found"). Without this branch, the missing-
--      secret case fell back to "unspecified" / "allow_path_security
--      _event" — uninformative for compliance grep.
--   5. "unspecified" — last resort.
local function security_reason(d, explicit)
    if explicit and explicit ~= "" then return explicit end
    if d.inject_dropped then return d.inject_dropped end
    if d.reason         then return d.reason end
    local skipped = d.injected_skipped
    if type(skipped) == "table" and skipped[1] and skipped[1].reason then
        return skipped[1].reason
    end
    return "unspecified"
end

-- Generate a request_id. We don't have ngx.var.request_id wired in
-- nginx.conf and it's not mandated to be RFC-uuid; what
-- matters is uniqueness within a worker for log correlation.
-- Fallback chain: ngx.var.request_id (if module enabled) → connection
-- id + connection_requests pair.
local function build_request_id()
    local rid = ngx.var.request_id
    if rid and rid ~= "" then return rid end
    local cn = ngx.var.connection or "0"
    local cr = ngx.var.connection_requests or "0"
    return cn .. "-" .. cr
end

-- Pull the inbound request headers we need to redact and audit.
-- ngx.req.get_headers() returns a metatable-wrapped table whose keys
-- are case-insensitive on access; we materialise it into a plain
-- table with original-case keys via the implicit pairs() (OpenResty
-- preserves the on-wire case in the underlying storage).
local function snapshot_request_headers()
    local h = ngx.req.get_headers(100, true) -- 100 max, raw=true
    if type(h) ~= "table" then return {} end
    local copy = {}
    for k, v in pairs(h) do
        copy[k] = v
    end
    return copy
end

-- Pull a User-Agent specifically; survives in audit even when the
-- user-agent header was redacted by name (it isn't, but defensively).
local function user_agent()
    local h = ngx.var.http_user_agent
    if not h or h == "" then return nil end
    return h
end

-- Project the credentials block from a decision struct. Used by
-- BOTH the merged http_request line and the dedicated security_event
-- line, so the two stay symmetric. Returns nil when no injects ran
-- (cjson.safe encodes nil as a missing key, matching "null
-- when nothing to report" convention).
--
-- Header names + secret_ids only — NEVER rendered values.
-- access_phase apply_injects() guarantees decision.injected_headers
-- carries no values; we re-assert the contract here by selecting
-- only the {header, secret_ref} fields.
local function credentials_block(d)
    local headers = d.injected_headers
    if type(headers) ~= "table" or #headers == 0 then return nil end
    local injected_names, secret_ids = {}, {}
    for i, e in ipairs(headers) do
        injected_names[i] = e.header
        secret_ids[i]     = e.secret_ref
    end
    return { injected = injected_names, secret_ids = secret_ids }
end

-- Compose the record. Reads everything from ngx.ctx.cube_decision
-- (written by access_phase) plus ngx.var.* observable in log_by_lua.
local function build_record(event)
    local d = ngx.ctx.cube_decision or {}

    local req_time    = tonumber(ngx.var.request_time)    or 0
    local server_port = tonumber(ngx.var.server_port)     or 0
    local req_len     = tonumber(ngx.var.request_length)  or 0
    local bytes_sent  = tonumber(ngx.var.bytes_sent)      or 0
    local status      = tonumber(ngx.var.status)          or 0

    -- security block: only populated when something interesting happened.
    -- null when nothing to report.
    local security = nil
    if d.security_event then
        security = {
            reason         = security_reason(d),
            inject_dropped = d.inject_dropped,
            inject_skipped = d.injected_skipped,
        }
    end

    return {
        ts         = ngx.var.time_iso8601,
        request_id = build_request_id(),
        event      = event or "http_request",
        sandbox = {
            src_ip    = ngx.var.remote_addr,
            policy_id = d.policy_id,
        },
        conn = {
            original_dst_ip   = ngx.var.server_addr,
            original_dst_port = server_port,
        },
        tls = {
            sni             = ngx.var.ssl_server_name,
            cipher          = ngx.var.ssl_cipher,
            version         = ngx.var.ssl_protocol,
            -- client_alpn / upstream_verified / upstream_cert_*
            -- stay nil rather than pretending we measured them.
            client_alpn           = ngx.var.ssl_alpn_protocol,
            upstream_verified     = nil,
            upstream_cert_subject = nil,
            upstream_cert_issuer  = nil,
        },
        http = {
            method     = ngx.var.request_method,
            host       = ngx.var.cube_audit_host,
            path       = ngx.var.request_uri,
            status     = status,
            req_bytes  = req_len,
            resp_bytes = bytes_sent,
            user_agent = user_agent(),
        },
        policy = {
            matched_rule = d.rule_id,
            decision     = (d.allow == true) and "allow"
                        or (d.allow == false) and "deny"
                        or "unknown",
            duration_us  = math.floor(req_time * 1000000),
        },
        credentials = credentials_block(d),
        security = security,
        -- populate these from header/body filters when
        -- audit_level == "full". For now the fields exist for schema
        -- stability but stay null.
        redacted_request_headers  = redactor.redact_headers(
                                        snapshot_request_headers()),
        redacted_request_body     = nil,
        redacted_response_body    = nil,
    }
end

-- ---------- IO ----------

-- Append one JSONL line to FILE_PATH. Returns true on success,
-- false + reason on failure. Caller decides severity.
local function append_line(line)
    local f, oerr = io.open(FILE_PATH, "a")
    if not f then return false, "open:" .. tostring(oerr) end
    local ok, werr = f:write(line, "\n")
    f:close()
    if not ok then return false, "write:" .. tostring(werr) end
    return true
end

-- ---------- public: main http_request line (called from log_by_lua) ----------

function _M.write_one()
    local rec = build_record("http_request")
    -- detect upstream handshake/connect failure on the allow path.
    -- An allowed request that returns 502/504 with zero upstream bytes
    -- received means we never got a response — most commonly the
    -- upstream cert verify failed (proxy_ssl_verify on),
    -- the dst IP refused TCP, or the upstream's TLS handshake aborted.
    -- We can't distinguish cert-failure from connect-refused here without
    -- parsing nginx's error log, but the audit consumer can do that
    -- offline if needed. The signal that matters for compliance is
    -- "credentials may have been injected toward an unverifiable
    -- upstream" — which is exactly what allow + 502 + zero-bytes means.
    local d = ngx.ctx.cube_decision or {}
    local status_n   = tonumber(ngx.var.status) or 0
    local up_recv    = tonumber(ngx.var.upstream_bytes_received) or 0
    local upstream_failed = d.allow == true
                            and (status_n == 502 or status_n == 504)
                            and up_recv == 0
    if upstream_failed then
        -- Mark on the in-progress record so the http_request line
        -- itself carries the security block, AND fire a dedicated
        -- security_event line below.
        rec.security = rec.security or {}
        rec.security.reason = rec.security.reason
                              or "upstream_unreachable_or_unverified"
        rec.security.upstream_status = ngx.var.upstream_status
        rec.security.upstream_addr   = ngx.var.upstream_addr
    end
    local line, err = cjson.encode(rec)
    if not line then
        ngx.log(ngx.ERR, "audit encode failed: ", err)
        return
    end
    local ok, ferr = append_line(line)
    if not ok then
        -- audit-not-writable is fatal at init_worker time,
        -- but post-init we degrade to ERR rather than os.exit (a
        -- worker exiting mid-request would corrupt other in-flight
        -- requests). The worker process supervisor will notice the
        -- repeated ERR and may restart it.
        ngx.log(ngx.ERR, "audit write failed: ", ferr)
    end
    -- Dedicated security_event line for upstream failures on the allow path.
    -- We don't mutate ngx.ctx.cube_decision here — it's already been written
    -- and the http_request line we emitted above already carries the
    -- security block. The dedicated line is for grep-by-event.
    if upstream_failed then
        _M.write_security_event("upstream_unreachable_or_unverified", d)
    end
end

-- ---------- public: out-of-band security_event (called from access_phase
--             deny path BEFORE ngx.exit) ----------
--
-- Security events get an extra dedicated line in addition
-- to the merged http_request line. Reason: log_by_lua may not run on
-- some failure paths (e.g. ngx.exit before access phase completes
-- cleanly is fine, but a Lua error in access would skip log_by_lua
-- entirely). The dedicated line is our "we definitely captured this"
-- guarantee for compliance.

function _M.write_security_event(reason, decision)
    local d = decision or ngx.ctx.cube_decision or {}
    local rec = {
        ts         = ngx.var.time_iso8601,
        request_id = build_request_id(),
        event      = "security_event",
        sandbox = {
            src_ip    = ngx.var.remote_addr,
            policy_id = d.policy_id,
        },
        conn = {
            original_dst_ip   = ngx.var.server_addr,
            original_dst_port = tonumber(ngx.var.server_port) or 0,
        },
        tls = {
            sni     = ngx.var.ssl_server_name,
            cipher  = ngx.var.ssl_cipher,
            version = ngx.var.ssl_protocol,
        },
        http = {
            method = ngx.var.request_method,
            host   = ngx.var.http_host,
            path   = ngx.var.request_uri,
        },
        -- When an allow-path security_event coexists
        -- with successful injects (e.g. one secret_ref missing while
        -- the others rendered fine), the merged http_request line and
        -- this dedicated line should agree on which credentials
        -- actually flowed upstream. Without this projection the
        -- dedicated line under-reported reality.
        credentials = credentials_block(d),
        policy = {
            matched_rule = d.rule_id,
            decision     = (d.allow == true) and "allow" or "deny",
        },
        security = {
            reason         = security_reason(d, reason),
            inject_dropped = d.inject_dropped,
            inject_skipped = d.injected_skipped,
        },
    }
    local line, err = cjson.encode(rec)
    if not line then
        ngx.log(ngx.ERR, "security_event encode failed: ", err)
        return
    end
    local ok, ferr = append_line(line)
    if not ok then
        ngx.log(ngx.ERR, "security_event write failed: ", ferr)
    end
end

-- ---------- public: TLS handshake failure (called from
--             ssl_certificate_by_lua_block on cert_signer error) ----------
--
-- cert_signer failure aborts the handshake before HTTP is parsed,
-- so log_by_lua never runs and write_one() / write_security_event()
-- can't fire. We emit a dedicated "tls_handshake" event here.
--
-- ngx.var.* surface in ssl_certificate_by_lua is mostly unpopulated
-- (no request yet). The caller passes sni + dst_ip + reason explicitly.
-- ts uses ngx.now() because ngx.var.time_iso8601 is not available
-- in this context.
--
-- Note: this is the only audit path that does NOT have a request_id
-- (none has been generated yet). We emit "tls-<connection>" so the
-- line is still groupable.
local function iso8601_from_ngx_now()
    -- ngx.now() returns Unix time in seconds with msec precision.
    local t = ngx.now()
    local sec = math.floor(t)
    local ms  = math.floor((t - sec) * 1000)
    -- !%FT%T = UTC ISO8601 yyyy-mm-ddTHH:MM:SS
    return string.format("%s.%03dZ", os.date("!%FT%T", sec), ms)
end

function _M.write_tls_handshake_event(sni, dst_ip, reason)
    local rec = {
        ts         = iso8601_from_ngx_now(),
        request_id = "tls-" .. tostring(ngx.var.connection or "0"),
        event      = "tls_handshake",
        sandbox = {
            src_ip = ngx.var.remote_addr,
        },
        conn = {
            original_dst_ip = dst_ip,
            -- server_port is available on the listening socket even
            -- in ssl phase (it's the listener port we accepted on).
            original_dst_port = tonumber(ngx.var.server_port) or 0,
        },
        tls = {
            sni = sni,
        },
        security = {
            reason = reason or "tls_handshake_failed",
        },
    }
    local line, err = cjson.encode(rec)
    if not line then
        ngx.log(ngx.ERR, "tls_handshake encode failed: ", err)
        return
    end
    local ok, ferr = append_line(line)
    if not ok then
        ngx.log(ngx.ERR, "tls_handshake write failed: ", ferr)
    end
end

return _M
