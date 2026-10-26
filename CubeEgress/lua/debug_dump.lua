-- CubeSandbox cube-egress — request/response header dump for debugging.
--
-- Gated by the `CUBE_EGRESS_DEBUG_DUMP` env var (default OFF). When the
-- gate is off, the exported functions are no-op closures so the per-request
-- overhead is a single function call returning immediately.
--
-- When ON, two lines per request land in nginx error_log (-> /dev/stderr,
-- visible via `docker logs -f cube-egress`):
--
--   [cube-debug] >>> request (sandbox -> upstream)
--   [cube-debug] <<< response (upstream -> sandbox)
--
-- Each block is terminated with a `--- end ---` sentinel because nginx
-- error_log appends its own context (`, client: ..., server: ...`) to the
-- last line of every ngx.log payload — without the sentinel that suffix
-- attaches to a real header value and looks like data corruption.
--
-- Request-body and response-body content are intentionally NOT dumped:
-- bodies can be large (streaming endpoints, LLM responses), can contain
-- credentials, and are already covered by the audit subsystem in a
-- redactor-aware way (lua/audit.lua + lua/redactor.lua). This module is
-- a debugging affordance, not a logging contract; do NOT enable
-- CUBE_EGRESS_DEBUG_DUMP=1 on shared / production hosts because the
-- header dump still bypasses the redactor and will print Authorization,
-- Cookie, and similar sensitive values verbatim.
--
-- Wired from nginx.conf:
--   access_by_lua_block        { require("debug_dump").dump_request("http"|"https") }
--   header_filter_by_lua_block { require("debug_dump").dump_response_headers() }

local _M = {}

local enabled = (os.getenv("CUBE_EGRESS_DEBUG_DUMP") == "1")

if not enabled then
    function _M.dump_request() end
    function _M.dump_response_headers() end
    return _M
end

-- Cap header count at 100 (ngx default) and force the rare-but-possible
-- duplicate-header case to produce a table rather than throwing on
-- get_headers/get_resp_headers internals (`raw_header_count > max`).
local HEADER_LIMIT = 100

local function format_headers(headers)
    if not headers then return "  (none)" end
    local keys = {}
    for k, _ in pairs(headers) do
        keys[#keys + 1] = k
    end
    table.sort(keys)
    local lines = {}
    for _, k in ipairs(keys) do
        local v = headers[k]
        if type(v) == "table" then
            v = table.concat(v, ", ")
        end
        lines[#lines + 1] = "  " .. k .. ": " .. tostring(v)
    end
    if #lines == 0 then
        return "  (none)"
    end
    return table.concat(lines, "\n")
end

function _M.dump_request(scheme)
    local headers = ngx.req.get_headers(HEADER_LIMIT, true)
    local sni_line = ""
    if scheme == "https" then
        sni_line = "\n  sni=" .. (ngx.var.ssl_server_name or "")
    end
    ngx.log(ngx.INFO,
        "\n[cube-debug] >>> request (sandbox -> upstream)",
        "\n  scheme=", scheme,
        " method=", ngx.var.request_method or "",
        " host=", ngx.var.http_host or "",
        " uri=", ngx.var.request_uri or "",
        "\n  src=", ngx.var.remote_addr or "", ":", ngx.var.remote_port or "",
        " dst=", ngx.var.server_addr or "", ":", ngx.var.server_port or "",
        sni_line,
        "\n  content-length=", ngx.var.http_content_length or "0",
        "\nheaders:\n", format_headers(headers),
        "\n--- end ---")
end

function _M.dump_response_headers()
    local headers = ngx.resp.get_headers(HEADER_LIMIT, true)
    local cl
    if headers then
        cl = headers["content-length"] or headers["Content-Length"]
        if type(cl) == "table" then cl = cl[1] end
    end
    ngx.log(ngx.INFO,
        "\n[cube-debug] <<< response (upstream -> sandbox)",
        "\n  status=", ngx.status or "",
        " content-length=", cl or "?",
        "\nheaders:\n", format_headers(headers),
        "\n--- end ---")
end

return _M
