-- CubeSandbox cube-egress — access-phase decision + credential injection middleware.
--
-- Responsibilities:
--   1. Build the request context from ngx.var (sandbox_ip, sni, host,
--      method, path, dst_ip, scheme).
--   2. Look up the policy for the sandbox source IP.
--   3. Walk policy.rules in order, first-match-wins.
--   4. On allow: enforce gates G1 (scheme is http or https) and
--      G4 (HTTPS: Host == SNI; HTTP: Host present), then for each
--      inject{header, secret, format} run
--      ngx.req.set_header. The secret value is embedded inline in the
--      policy entry — no external lookup —
--      so the only inject-failure shape left is "policy malformed at
--      decision time" (e.g. inj.secret is nil because a stale entry
--      was somehow loaded). That case drops just that one inject and
--      flags security_event.
--   5. Stash the decision in ngx.ctx.cube_decision for audit.
--   6. On deny, return 403 here without touching upstream.
--
-- Gate ownership in this module:
--   G1 scheme valid     — checked here (ctx.scheme is "http" or "https")
--   G2 TLS handshake OK — implicit (request reached HTTP layer)
--   G3 SNI matches rule — by rule_matches() above (HTTPS only; HTTP has no SNI)
--   G4 Host == SNI      — checked here, before any inject
--                         (HTTPS: Host==SNI; HTTP: Host present)
--   G5 dst IP authentic — implicit via G6 (proxy_ssl_verify)
--   G6 upstream cert    — Pδ; nginx proxy_ssl_verify on (already in
--                         http {}-level config)
--   G7 method/path      — by rule_matches()
--   G8 policy authorise — implicit (we are inside the matched rule)
--
-- "any G1-G8 failure → drop inject; the request continues
-- based on action.allow". That is what we do: a G4 mismatch on an
-- otherwise allowed rule means we proxy the traffic upstream WITHOUT
-- the secret, and tag the decision as a security_event.
--
-- Match-field semantics (all optional; missing field → matches anything):
--   sni             string (exact or leading "*." wildcard, case-insensitive)
--   host            string (exact or leading "*." wildcard, case-insensitive; HTTP Host)
--   method          array of strings (any-of)
--   path            string (exact or trailing "*" prefix wildcard on ngx.var.uri)
--   scheme          "http" | "https"
--
-- Bootstrap-status semantics:
--   "ready"   — bootstrap fetched successfully; enforce policy.
--   "skipped" — CUBE_EGRESS_BOOTSTRAP_URL unset (dev / standalone
--               mode); enforce policy. policy_store starts empty,
--               so default-deny applies until admin API loads
--               policies. This lets local tests exercise the full
--               admin → access_phase path without needing an
--               external admin server.
--   "pending" — bootstrap fetch in flight (init_worker scheduled
--               a timer that hasn't completed). Fail CLOSED: return
--               403. CubeEgress is the outbound security boundary
--               for untrusted sandbox code, so serving traffic
--               without policies would let a sandbox bypass
--               host/SNI/method/path controls during every startup
--               or restart window. bootstrap.lua's own header says
--               cube-egress MUST NOT serve data-plane traffic
--               without policies; this gate enforces that invariant.
--   "unknown" — meta_store unavailable (shouldn't happen). Same
--               fail-closed as "pending"; a missing meta_store means
--               we also can't safely look up policies.
--   any other — treated like "unknown" for safety.
--
-- Production deployments MUST set CUBE_EGRESS_BOOTSTRAP_URL so the
-- worker reaches "ready" before serving traffic. Dev/standalone
-- deployments use "skipped" + admin API for policy installation.

local policy = require "policy"

local _M = {}

-- ---------- helpers ----------

local function lower(s)
    if type(s) ~= "string" then return nil end
    return string.lower(s)
end

-- Strip an optional ":port" off an HTTP Host header for comparison.
local function host_without_port(h)
    if type(h) ~= "string" then return nil end
    -- IPv6 hosts are bracketed; we don't support v6 anyway, but be
    -- defensive: don't accidentally split inside brackets.
    if string.sub(h, 1, 1) == "[" then return h end
    local i = string.find(h, ":", 1, true)
    if i then return string.sub(h, 1, i - 1) end
    return h
end

local function suffix_match(s, suffix)
    if not s or not suffix then return false end
    local sl, fl = #s, #suffix
    if fl > sl then return false end
    return string.sub(s, sl - fl + 1) == suffix
end

local function domain_match(pattern, value)
    pattern = lower(pattern)
    value = lower(value)
    if not pattern or not value then return false end
    -- Match cubevs DNS allow semantics: only leading "*." is a wildcard,
    -- and it matches subdomains only, not the apex domain.
    if string.sub(pattern, 1, 2) == "*." and #pattern > 2 then
        return suffix_match(value, string.sub(pattern, 2))
    end
    return pattern == value
end

local function path_match(pattern, value)
    if type(pattern) ~= "string" or type(value) ~= "string" then return false end
    -- Path supports exact match by default, and a single trailing "*"
    -- for prefix match. Unlike sni/host wildcards, the wildcard direction
    -- is suffix-side because paths grow to the right.
    if string.sub(pattern, -1) == "*" then
        local prefix = string.sub(pattern, 1, -2)
        return string.sub(value, 1, #prefix) == prefix
    end
    return pattern == value
end

-- ---------- match evaluation ----------

-- Returns true if every constraint in `m` passes against `ctx`.
local function rule_matches(m, ctx)
    if type(m) ~= "table" then return false end

    if m.sni ~= nil then
        if not domain_match(m.sni, ctx.sni) then return false end
    end
    if m.host ~= nil then
        if not domain_match(m.host, ctx.host) then return false end
    end
    if m.method ~= nil then
        if type(m.method) ~= "table" then return false end
        local hit = false
        for _, mm in ipairs(m.method) do
            if string.upper(mm) == ctx.method then hit = true; break end
        end
        if not hit then return false end
    end
    if m.path ~= nil then
        if not path_match(m.path, ctx.path) then return false end
    end
    if m.scheme ~= nil then
        if string.lower(m.scheme) ~= ctx.scheme then return false end
    end
    return true
end

-- ---------- injection ----------

-- Render a secret value into a header string by replacing "${SECRET}"
-- with the raw secret. Default format is bare "${SECRET}", i.e. the
-- header value is the secret itself (e.g. for X-API-Key style headers).
-- Defensive: if the format does not contain ${SECRET} we treat the
-- whole format as a literal AND log WARN — operators almost never want
-- a constant header value, so this catches typos like "Bearer $SECRET".
local function render_inject(format, secret_value)
    local fmt = format
    if type(fmt) ~= "string" or fmt == "" then
        fmt = "${SECRET}"
    end
    -- string.gsub treats "%" specially in the replacement string
    -- (%0..%9 reference capture groups). Secret values may legitimately
    -- contain "%" (random base64-y tokens do), so escape "%" → "%%"
    -- before substitution. The `%${SECRET}` pattern matches literal
    -- "${SECRET}" because `%$` is an escaped dollar sign.
    local escaped = string.gsub(secret_value, "%%", "%%%%")
    local rendered, n = string.gsub(fmt, "%${SECRET}", escaped, 1)
    if n == 0 then
        ngx.log(ngx.WARN, "[inject] format has no ${SECRET} placeholder: ",
                          "'", fmt, "' — treating as literal header value. ",
                          "Did you mean to write \"${SECRET}\"?")
    end
    return rendered
end

-- Run G1-G4 gates that protect the inject path. Returns
--   true                      → all gates passed; safe to inject
--   false, "reason"           → drop ALL injects for this request
--   false, "reason", true     → drop ALL injects AND mark security_event
local function inject_gates(ctx)
    -- G1: scheme must be either "http" or "https". We now allow inject
    -- on plain HTTP as well as HTTPS — operators may legitimately need
    -- to inject credentials into plaintext traffic on trusted networks
    -- (e.g. intra-cluster 80). Any other scheme value means something
    -- unexpected (ngx.var.scheme is http for the :8080 server block and
    -- https for :8443, so this branch is only hit on misconfig); drop
    -- inject, but do not flag security_event.
    if ctx.scheme ~= "http" and ctx.scheme ~= "https" then
        return false, "g1_unsupported_scheme", false
    end
    -- G4: HTTP Host must equal the SNI (HTTPS) or be present (HTTP).
    --
    --   HTTPS: SNI is what we used to pick the rule and what
    --   proxy_ssl_name will send to the upstream. A mismatch means the
    --   sandbox is trying to convince us "talk to host A's cert/SNI but
    --   route the request as if Host=B" — exactly the request-smuggling
    --   shape we want to block.
    --
    --   HTTP: there is no SNI, so the Host header is the only routing
    --   identity. rule_matches() has already validated Host against the
    --   rule's match constraint, so we only need to assert Host is
    --   present — an empty Host on an otherwise-matched inject rule is
    --   a misconfig / smuggling attempt and drops the inject.
    if ctx.scheme == "https" then
        if not ctx.sni or not ctx.host then
            return false, "g4_missing_sni_or_host", true
        end
        if string.lower(ctx.sni) ~= string.lower(ctx.host) then
            return false, "g4_host_sni_mismatch", true
        end
    else  -- ctx.scheme == "http"
        if not ctx.host or ctx.host == "" then
            return false, "g4_missing_host", true
        end
    end
    return true
end

local function apply_injects(decision, inject_list)
    -- With inline secrets there is no longer an
    -- out-of-band lookup that can miss. The only inject-time failures
    -- are (a) malformed inject directive (missing header / missing
    -- inline secret on a stored policy that escaped validation
    -- somehow), or (b) ngx.req.set_header throwing. Both are operator
    -- misconfig: drop that one inject + flag security_event but DO
    -- NOT block the request.
    --
    -- Audit invariant: every applied/skipped record carries a
    -- `secret_ref` field whose value is the synthetic fingerprint
    -- stamped by policy.put. Redaction logic and the
    -- `secret_ids` audit array key off that field name unchanged.
    local applied = {}
    local skipped = {}
    for _, inj in ipairs(inject_list) do
        local header     = inj.header
        local secret_val = inj.secret
        local secret_ref = inj.secret_ref_synthetic or "fp-unknown"
        local fmt        = inj.format
        if type(header) ~= "string" or header == ""
           or type(secret_val) ~= "string" or secret_val == "" then
            skipped[#skipped + 1] = {
                secret_ref = secret_ref,
                reason     = "secret_missing_in_policy",
            }
            ngx.log(ngx.ERR, "[inject] malformed inject directive for sandbox=",
                              tostring(decision.sandbox_ip),
                              " rule=", tostring(decision.rule_id),
                              " secret_ref=", secret_ref)
        else
            local rendered = render_inject(fmt, secret_val)
            local ok, serr = pcall(ngx.req.set_header, header, rendered)
            if not ok then
                ngx.log(ngx.ERR, "[inject] set_header failed: ", serr)
                skipped[#skipped + 1] = {
                    secret_ref = secret_ref,
                    reason     = "set_header_failed",
                }
            else
                -- Record the header name + synthetic secret_ref for
                -- audit. NEVER record the rendered value or the raw
                -- secret (Redaction depends on us not having
                -- either in ctx).
                applied[#applied + 1] = {
                    header     = header,
                    secret_ref = secret_ref,
                }
            end
        end
    end
    decision.injected_headers = applied
    decision.injected_skipped = skipped
    if #skipped > 0 then
        -- Operator misconfig is a security_event class so monitoring
        -- can alert on it (an env that *should* have a secret but
        -- doesn't is a credential-leak risk: traffic flows w/o auth).
        decision.security_event = true
    end
end



-- ---------- decision core ----------

local function build_ctx()
    local dst_ip = ngx.var.server_addr
    return {
        sandbox_ip   = ngx.var.remote_addr,
        sni          = ngx.var.ssl_server_name,    -- nil for :8080 plain HTTP
        host         = host_without_port(ngx.var.http_host),
        method       = ngx.var.request_method,
        path         = ngx.var.uri,
        dst_ip       = dst_ip,
        scheme       = ngx.var.scheme,
    }
end

local function deny(reason, decision)
    decision.allow  = false
    decision.reason = reason
    -- Stash for log_by_lua / audit.
    ngx.ctx.cube_decision = decision
    -- Phase β minimal log line; kept even after Pε wires structured
    -- audit because deny is a low-volume event and a single-line ERR
    -- log is convenient for operators tailing /dev/stderr.
    ngx.log(ngx.NOTICE, "[access] DENY sandbox=", tostring(decision.sandbox_ip),
                       " policy=", tostring(decision.policy_id),
                       " rule=", tostring(decision.rule_id),
                       " reason=", reason)
    -- security_event class denies get a dedicated JSONL line in
    -- addition to the http_request line that log_by_lua will write.
    -- The dedicated line is our "definitely captured" guarantee even
    -- if log_by_lua is somehow skipped.
    if decision.security_event then
        local ok, audit = pcall(require, "audit")
        if ok and audit and audit.write_security_event then
            -- pcall the call too — a broken audit module must not
            -- prevent the deny from happening.
            pcall(audit.write_security_event, reason, decision)
        end
    end
    return ngx.exit(403)
end

local function allow(decision)
    decision.allow = true
    -- If the matched rule wants credential injection, run G1/G4
    -- gates and then apply each inject directive. Failed gates drop
    -- ALL injects but the request still proceeds ("any G
    -- failure → drop inject; request continues based on action.allow").
    -- Failed gates flag security_event when the failure reason is
    -- sandbox-controlled (G4 mismatch, missing SNI/Host on https,
    -- missing Host on http).
    --
    -- Important: when the rule says "we plan to inject header X", we
    -- ALWAYS clear that header from the sandbox-provided request first,
    -- whether or not the gates pass. Otherwise a sandbox can race the
    -- gate by sending its own forged Authorization header and have it
    -- forwarded upstream when our gate fails. Clearing first means
    --   gate pass → ngx.req.set_header writes the real value
    --   gate fail → header is empty (sandbox's forgery is gone)
    local injects = decision.inject
    if type(injects) == "table" and #injects > 0 then
        for _, inj in ipairs(injects) do
            if type(inj.header) == "string" and inj.header ~= "" then
                -- ngx.req.clear_header is the documented way to remove
                -- a header from the request (vs set_header "" which
                -- leaves an empty-value header — semantically different
                -- for some servers).
                pcall(ngx.req.clear_header, inj.header)
            end
        end
        local ok, reason, sec = inject_gates({
            scheme = decision.scheme,
            sni    = decision.sni,
            host   = decision.host,
        })
        if not ok then
            decision.inject_dropped = reason
            if sec then decision.security_event = true end
            ngx.log(ngx.NOTICE, "[inject] gates failed reason=", reason,
                                 " sandbox=", tostring(decision.sandbox_ip),
                                 " sni=", tostring(decision.sni),
                                 " host=", tostring(decision.host))
        else
            apply_injects(decision, injects)
        end
    end
    ngx.ctx.cube_decision = decision
    -- If the gates / inject path flagged security_event on the
    -- ALLOW path (G4 mismatch, missing SNI/Host, missing secret_ref),
    -- emit the dedicated security_event line now. The http_request
    -- line will follow at log_by_lua time and will carry the security
    -- block too — both are required.
    --
    -- We do NOT exit the request here; gate failure drops the inject
    -- but the request is still allowed ("any G failure →
    -- drop inject; request continues based on action.allow").
    if decision.security_event then
        local ok_a, audit = pcall(require, "audit")
        if ok_a and audit and audit.write_security_event then
            -- Pass nil as the explicit reason so audit.security_reason()
            -- drives the fallback chain (inject_dropped → decision.reason
            -- → injected_skipped[1].reason → "unspecified"). This keeps
            -- the dedicated line's reason field in sync with what the
            -- merged http_request line will compute at log_by_lua, and
            -- preserves the missing-secret case's "secret_not_found"
            -- instead of collapsing to a generic placeholder.
            pcall(audit.write_security_event, nil, decision)
        end
    end
    -- Allow path stays quiet on stderr by default; the structured
    -- audit lines above are the source of truth.
    return  -- proceed with the request
end

-- Public: called from access_by_lua_block.
function _M.decide()
    local ctx = build_ctx()

    local decision = {
        sandbox_ip       = ctx.sandbox_ip,
        sni              = ctx.sni,
        host             = ctx.host,
        method           = ctx.method,
        path             = ctx.path,
        dst_ip           = ctx.dst_ip,
        scheme           = ctx.scheme,
        policy_id        = nil,
        rule_id          = nil,
        audit_level      = "metadata",
        inject           = nil,
        security_event   = false,
    }

    -- Bootstrap gate (see module header).
    -- "ready" and "skipped" both proceed to normal policy enforcement.
    -- "pending" / "unknown" / anything else fail CLOSED. CubeEgress is
    -- the documented outbound security boundary for untrusted sandbox
    -- code, so serving data-plane traffic before policies are loaded
    -- would let a sandbox exfiltrate data or bypass host/SNI/method/path
    -- controls during every startup or restart window. bootstrap.lua's
    -- own header says cube-egress MUST NOT serve data-plane traffic
    -- without policies; we enforce that invariant here.
    local meta = ngx.shared.meta_store
    local bootstrap_status = (meta and meta:get("bootstrap_status")) or "unknown"
    if bootstrap_status ~= "ready" and bootstrap_status ~= "skipped" then
        decision.security_event = true
        decision.reason = "bootstrap_not_ready:" .. bootstrap_status
        decision.audit_level = "metadata"
        ngx.ctx.cube_decision = decision
        -- Avoid log spam: only warn when this is a fresh worker. A
        -- shared_dict counter keeps it to one line per worker per status.
        if meta then
            local seen_key = "warn_seen:" .. bootstrap_status .. ":" .. (ngx.worker.id() or 0)
            local newly = meta:add(seen_key, 1)
            if newly then
                ngx.log(ngx.WARN, "[access] bootstrap_status=", bootstrap_status,
                                   "; denying all traffic until bootstrap ",
                                   "completes. Set CUBE_EGRESS_BOOTSTRAP_URL ",
                                   "and load policies for production behavior.")
            end
        end
        return ngx.exit(403)
    end

    if not ctx.sandbox_ip then
        decision.security_event = true
        return deny("no_remote_addr", decision)
    end

    local p, perr = policy.get(ctx.sandbox_ip)
    if perr then
        ngx.log(ngx.ERR, "[access] policy.get failed for ",
                          tostring(ctx.sandbox_ip), ": ", perr)
        decision.security_event = true
        return deny("policy_lookup_error", decision)
    end
    if not p then
        decision.security_event = true
        return deny("no_policy_for_sandbox", decision)
    end

    decision.policy_id = p.policy_id

    for _, r in ipairs(p.rules or {}) do
        if rule_matches(r.match, ctx) then
            decision.rule_id     = r.id
            decision.audit_level = (r.action and r.action.audit) or "metadata"
            decision.inject      = r.action and r.action.inject  -- consumed in Pγ
            if r.action and r.action.allow == true then
                return allow(decision)
            else
                return deny("rule_deny", decision)
            end
        end
    end

    -- No rule matched and policy provided no default → deny.
    decision.security_event = true
    return deny("no_rule_match", decision)
end

-- Exposed for unit-testing the matcher in isolation.
_M._rule_matches = rule_matches

return _M
