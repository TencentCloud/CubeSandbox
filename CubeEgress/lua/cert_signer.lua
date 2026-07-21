-- CubeSandbox transparent proxy — in-process MITM cert signer.
--
-- - CA cert+key loaded once at init_by_lua, kept in module-level locals
--   (inherited by all workers via fork).
-- - Per-SNI leaf cert signed lazily, cached in lua_shared_dict cert_cache.
-- - Stampede protected by lua-resty-lock on cert_locks.
-- - ECDSA P-256 leaf, configurable validity / cache TTL.
--
-- Cache value layout (kept compact to minimize shared_dict footprint):
--   4-byte big-endian cert_len | cert_der | key_der
--
-- Required shared dicts (declared in nginx.conf):
--   lua_shared_dict cert_cache 64m;
--   lua_shared_dict cert_locks 8m;

local ssl         = require "ngx.ssl"
local resty_lock  = require "resty.lock"
local pkey_lib    = require "resty.openssl.pkey"
local x509_lib    = require "resty.openssl.x509"
local name_lib    = require "resty.openssl.x509.name"
local altname_lib = require "resty.openssl.x509.altname"
local ext_lib     = require "resty.openssl.x509.extension"
local digest_lib  = require "resty.openssl.digest"
local bn_lib      = require "resty.openssl.bn"

local _M = {}

-- Module-level state set by bootstrap()
local CA_CERT       -- resty.openssl.x509
local CA_KEY        -- resty.openssl.pkey
local LEAF_TTL_SEC
local CACHE_TTL_SEC

local function read_file(path)
    local f, err = io.open(path, "rb")
    if not f then return nil, err end
    local data = f:read("*a")
    f:close()
    return data
end

-- Crude but sufficient: SNI is almost never an IPv6 literal; this catches
-- IPv4 dotted quads and anything containing a colon (treated as IPv6).
local function is_ip_literal(s)
    if not s then return false end
    if s:match("^%d+%.%d+%.%d+%.%d+$") then return true end
    if s:find(":", 1, true) then return true end
    return false
end

-- 4-byte big-endian length prefix + payload(s)
local function pack_entry(cert_der, key_der)
    local clen = #cert_der
    local prefix = string.char(
        math.floor(clen / 16777216) % 256,
        math.floor(clen / 65536) % 256,
        math.floor(clen / 256) % 256,
        clen % 256)
    return prefix .. cert_der .. key_der
end

local function unpack_entry(entry)
    local b1, b2, b3, b4 = string.byte(entry, 1, 4)
    local clen = b1 * 16777216 + b2 * 65536 + b3 * 256 + b4
    return entry:sub(5, 4 + clen), entry:sub(5 + clen)
end

-- ---- Public: bootstrap (called from init_by_lua) ----
function _M.bootstrap(opts)
    LEAF_TTL_SEC  = opts.leaf_ttl_sec  or (7 * 86400)
    CACHE_TTL_SEC = opts.cache_ttl_sec or (6 * 86400)

    local cert_pem, err = read_file(opts.ca_cert_path)
    if not cert_pem then error("read CA cert failed: " .. tostring(err)) end
    local key_pem, err2 = read_file(opts.ca_key_path)
    if not key_pem then error("read CA key failed: " .. tostring(err2)) end

    local ca_cert, e1 = x509_lib.new(cert_pem, "PEM")
    if not ca_cert then error("parse CA cert failed: " .. tostring(e1)) end
    local ca_key, e2 = pkey_lib.new(key_pem, { format = "PEM" })
    if not ca_key then error("parse CA key failed: " .. tostring(e2)) end

    -- Best-effort sanity check; not all resty.openssl versions expose this.
    if ca_cert.check_private_key then
        local ok, e3 = ca_cert:check_private_key(ca_key)
        if not ok then error("CA cert/key mismatch: " .. tostring(e3)) end
    end

    CA_CERT = ca_cert
    CA_KEY  = ca_key

    ngx.log(ngx.INFO, "cert_signer bootstrapped, leaf_ttl=", LEAF_TTL_SEC,
                      "s cache_ttl=", CACHE_TTL_SEC, "s")
end

-- ---- Internal: sign a fresh leaf for (sni, dst_ip) ----
local function sign_leaf(sni, dst_ip)
    -- 1. Generate leaf key (ECDSA P-256: fast, small)
    local leaf_key, err = pkey_lib.new({ type = "EC", curve = "prime256v1" })
    if not leaf_key then return nil, "gen leaf key: " .. tostring(err) end

    -- 2. Build cert
    local cert = x509_lib.new()
    cert:set_version(3)

    -- Random 64-bit serial; uniqueness is best-effort for short-lived MITM certs.
    local serial = bn_lib.new(math.floor(ngx.now() * 1000000) + math.random(1, 1e9))
    if serial then cert:set_serial_number(serial) end

    local now = ngx.time()
    cert:set_not_before(now - 60)              -- 60s clock skew tolerance
    cert:set_not_after(now + LEAF_TTL_SEC)

    -- Subject CN
    local subj = name_lib.new()
    subj:add("CN", sni or "unknown")
    cert:set_subject_name(subj)

    -- Issuer = CA subject
    cert:set_issuer_name(CA_CERT:get_subject_name())

    cert:set_pubkey(leaf_key)

    -- SAN: prefer SNI; fall back to dst IP if no SNI.
    local sans = altname_lib.new()
    if sni and not is_ip_literal(sni) then
        sans:add("DNS", sni)
    elseif sni and is_ip_literal(sni) then
        sans:add("IP", sni)
    elseif dst_ip then
        sans:add("IP", dst_ip)
    end
    local ok_san, err_san = cert:set_subject_alt_name(sans)
    if not ok_san then return nil, "set SAN: " .. tostring(err_san) end

    cert:add_extension(ext_lib.new("basicConstraints", "critical,CA:FALSE"))
    cert:add_extension(ext_lib.new("keyUsage",
        "critical,digitalSignature,keyEncipherment"))
    cert:add_extension(ext_lib.new("extendedKeyUsage", "serverAuth"))

    -- 3. Sign with CA
    local ok, e2 = cert:sign(CA_KEY, digest_lib.new("sha256"))
    if not ok then return nil, "sign cert: " .. tostring(e2) end

    -- 4. DER (what ngx.ssl.set_der_* wants)
    local cert_der, e3 = cert:tostring("DER")
    if not cert_der then return nil, "cert DER: " .. tostring(e3) end

    -- pkey:tostring(private_or_public, format) — note the arg order differs
    -- from x509:tostring(format). Swapping these gives the misleading error
    -- "can only export private or public key, not DER" because the lib treats
    -- the first arg as the type and "DER" matches neither "PrivateKey" nor
    -- "PublicKey".
    local key_der, e4 = leaf_key:tostring("PrivateKey", "DER")
    if not key_der then return nil, "key DER: " .. tostring(e4) end

    return cert_der, key_der
end

-- ---- Public: serve (called from ssl_certificate_by_lua_block) ----
function _M.serve(sni, dst_ip)
    -- Cache key: SNI when present, else "ip:<dst>".
    local key = sni or ("ip:" .. (dst_ip or "unknown"))
    local cache = ngx.shared.cert_cache

    local cert_der, key_der
    local entry = cache:get(key)

    if entry then
        cert_der, key_der = unpack_entry(entry)
    else
        -- Slow path: lock, double-check, sign, populate.
        local lock, lerr = resty_lock:new("cert_locks", { timeout = 5 })
        if not lock then return false, "lock new: " .. tostring(lerr) end

        local elapsed, lkerr = lock:lock(key)
        if not elapsed then return false, "lock acquire: " .. tostring(lkerr) end

        entry = cache:get(key)
        if entry then
            cert_der, key_der = unpack_entry(entry)
            lock:unlock()
        else
            local c, k_or_err = sign_leaf(sni, dst_ip)
            if not c then
                lock:unlock()
                return false, k_or_err
            end
            cert_der = c
            key_der  = k_or_err
            cache:set(key, pack_entry(cert_der, key_der), CACHE_TTL_SEC)
            lock:unlock()
        end
    end

    -- Replace nginx's about-to-be-used cert for this handshake.
    local ok, err = ssl.clear_certs()
    if not ok then return false, "clear_certs: " .. tostring(err) end

    ok, err = ssl.set_der_cert(cert_der)
    if not ok then return false, "set_der_cert: " .. tostring(err) end

    ok, err = ssl.set_der_priv_key(key_der)
    if not ok then return false, "set_der_priv_key: " .. tostring(err) end

    return true
end

return _M
