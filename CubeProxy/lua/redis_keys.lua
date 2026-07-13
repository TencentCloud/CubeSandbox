-- file name: redis_keys.lua
--
-- Centralized Redis key construction for CubeProxy. Keys follow the unified
-- convention "cube:{ver}:{scope}:{resource}[:{sub}]:{id}" shared with
-- CubeMaster. Reads always try the new key first and fall back to the legacy
-- key so a simultaneous upgrade can still route to pre-cutover data.
--
-- See docs/architecture/redis-key-spec.md for the full convention.

local _M = { _VERSION = "0.01" }

local PREFIX = "cube"
local VERSION = "v1"

-- Sandbox proxy routing metadata (shared with CubeMaster, written by it).
function _M.sandbox_proxy(ins_id)
    return PREFIX .. ":" .. VERSION .. ":shared:sandbox:proxy:" .. ins_id
end

function _M.legacy_sandbox_proxy(ins_id)
    return "bypass_host_proxy:" .. ins_id
end

-- read_keys_with_fallback returns keys to try on read: new first, legacy second.
function _M.read_keys_with_fallback(new_key, legacy_key)
    return { new_key, legacy_key }
end

return _M
