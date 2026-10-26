proxy_buffering off;
# Prevent nginx gzip-compressing Connect stream responses.
# iter_raw() does not decompress Content-Encoding — gzip
# magic bytes misread as 2.17 GiB Connect frame header.
gzip off;
# Streaming envd errors must pass through verbatim; intercepting them here
# would replace protocol-native error frames with nginx error pages and break
# client-side parsing.
proxy_intercept_errors off;

proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";

proxy_send_timeout 7206s;
proxy_read_timeout 7206s;
proxy_connect_timeout 3s;
proxy_set_header Host $cube_upstream_host;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Prefix /sandbox/$ins_id/$container_port;

proxy_redirect    ~^/(.*)$  /sandbox/$ins_id/$container_port/$1;
proxy_cookie_path /         /sandbox/$ins_id/$container_port/;

rewrite_by_lua_file lua/path_rewrite_phase.lua;

proxy_pass http://backend;

header_filter_by_lua_file lua/header_filter_phase.lua;

log_by_lua_file lua/log_phase.lua;
