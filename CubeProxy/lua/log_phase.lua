local function get_currtime()
    -- ngx.var.msec = 1663839717.105
    local current_time_seconds_with_ms = ngx.var.msec

    -- Get milliseconds part
    local current_ms = math.floor((current_time_seconds_with_ms * 1000) % 1000)

    -- Format
    local formatted_time = os.date("%Y-%m-%dT%H:%M:%S") .. "." .. current_ms
    return formatted_time
end

ngx.var.access_time = get_currtime()
