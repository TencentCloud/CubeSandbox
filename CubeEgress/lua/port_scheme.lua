-- CubeEgress L7 destination port/scheme semantics shared by validation and
-- request matching.
local _M = {}

local function trim(s)
    return (string.gsub(s, "^%s*(.-)%s*$", "%1"))
end

function _M.normalize_scheme(value)
    if value == nil then return nil end
    if type(value) ~= "string" then
        return nil, "scheme must be a string"
    end
    local scheme = string.lower(trim(value))
    if scheme ~= "http" and scheme ~= "https" then
        return nil, "scheme must be http or https"
    end
    return scheme
end

local function normalize_port(value)
    if value == nil then return nil end
    if type(value) ~= "number" or value ~= math.floor(value) then
        return nil, "port must be an integer"
    end
    if value < 1 or value > 65535 then
        return nil, "port must be in [1, 65535]"
    end
    return value
end

-- expand returns the effective destination tuples for a rule:
--   nil/nil      -> 80/http + 443/https
--   nil/scheme   -> the scheme's conventional port
--   port/scheme  -> the exact tuple
--   port/nil     -> invalid
function _M.expand(port_value, scheme_value)
    local port, perr = normalize_port(port_value)
    if perr then return nil, perr end
    local scheme, serr = _M.normalize_scheme(scheme_value)
    if serr then return nil, serr end

    if port ~= nil and scheme == nil then
        return nil, "port requires scheme"
    end
    if port ~= nil then
        return {{port = port, scheme = scheme}}
    end
    if scheme == "http" then
        return {{port = 80, scheme = "http"}}
    end
    if scheme == "https" then
        return {{port = 443, scheme = "https"}}
    end
    return {
        {port = 80, scheme = "http"},
        {port = 443, scheme = "https"},
    }
end

function _M.matches(port_value, scheme_value, dst_port, request_scheme)
    local tuples = _M.expand(port_value, scheme_value)
    if not tuples then return false end
    local scheme = _M.normalize_scheme(request_scheme)
    local port = tonumber(dst_port)
    if not scheme or not port then return false end

    for _, tuple in ipairs(tuples) do
        if tuple.port == port and tuple.scheme == scheme then
            return true
        end
    end
    return false
end

-- matches_deny evaluates the port/scheme constraint for a DENY rule. An allow
-- rule with an omitted port narrows to the default set {80/http,443/https}
-- (fail-closed: a custom-port flow needs an explicit allow). A deny rule with
-- an omitted port is instead port-AGNOSTIC within the host: it matches any
-- intercepted flow to the host. Narrowing a deny to the default set would be
-- fail-open — a custom-port allow rule (e.g. allow host="api.example.com"
-- port=8443 scheme=https) would bypass a broader host deny (deny
-- host="*.example.com") because the deny never matched port 8443. An
-- explicitly-set port+scheme still matches exactly that tuple.
function _M.matches_deny(port_value, scheme_value, dst_port, request_scheme)
    local port, perr = normalize_port(port_value)
    if perr then return false end
    local scheme, serr = _M.normalize_scheme(scheme_value)
    if serr then return false end
    if port ~= nil then
        -- Explicit port (+scheme): exact tuple, same as the allow path.
        return _M.matches(port_value, scheme_value, dst_port, request_scheme)
    end
    if scheme == nil then
        -- No port and no scheme: deny every intercepted flow to the host.
        return true
    end
    -- No port but a scheme: deny every intercepted flow of that scheme,
    -- regardless of which port carried it.
    local req_scheme = _M.normalize_scheme(request_scheme)
    return req_scheme ~= nil and req_scheme == scheme
end

return _M
