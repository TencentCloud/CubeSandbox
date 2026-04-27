# Task: Write cube_e2b Python SDK

## Goal
Write a clean Python SDK `cube_e2b` that wraps CubeSandbox's E2B-compatible API.
Key requirement: support `CUBE_PROXY_NODE_IP` env var to bypass fake DNS for data stream access.

## E2B OpenAPI Key Endpoints (from spec)
- POST /sandboxes → create sandbox, returns {sandboxID, templateID, clientID, envdVersion, domain}
- DELETE /sandboxes/{sandboxID} → kill sandbox
- GET /sandboxes → list sandboxes  
- GET /sandboxes/{sandboxID} → get sandbox detail
- POST /sandboxes/{sandboxID}/timeout → refresh timeout
- POST /sandboxes/{sandboxID}/pause → pause sandbox
- POST /sandboxes/{sandboxID}/resume → resume sandbox

## URL Resolution Logic (CRITICAL)
Sandbox service access domain format: `<port>-<sandboxID>.<domain>`
e.g. `49999-5405bd0b3b584ac6bafb7656ebe19f8c.cube.app`

When `CUBE_PROXY_NODE_IP` is set, bypass DNS entirely:
- HTTP: use `Host` header + direct IP connection
- WebSocket: connect to IP:80 with `Host` header set to domain
- HTTPS/WSS: connect to IP:443 with SNI + `Host` header

## Environment Variables
- `CUBE_API_URL` - CubeAPI address, default `http://127.0.0.1:3000`
- `E2B_API_KEY` - API key (any non-empty string for local deploy)
- `CUBE_TEMPLATE_ID` - default template ID
- `CUBE_PROXY_NODE_IP` - if set, bypass DNS, connect directly to this IP for data streams
- `CUBE_PROXY_PORT_HTTP` - HTTP port for proxy, default 80
- `CUBE_PROXY_PORT_HTTPS` - HTTPS port for proxy, default 443
- `SSL_CERT_FILE` - CA cert for mkcert self-signed certs

## SDK Structure
```
cube_e2b/
  __init__.py       - exports: Sandbox, SandboxConfig
  sandbox.py        - main Sandbox class
  client.py         - HTTP client wrapping CubeAPI REST
  stream.py         - data stream helpers (HTTP/WS with IP bypass)
  config.py         - config/env reading
  exceptions.py     - CubeSandboxError, SandboxNotFoundError, etc.
requirements.txt
example_create.py   - simple create/run/destroy example
example_stream.py   - WebSocket stream example
README.md
```

## Sandbox Class API
```python
class Sandbox:
    @classmethod
    def create(cls, template: str = None, timeout: int = 300, env_vars: dict = None, **kwargs) -> "Sandbox"
    
    def get_host(self, port: int) -> str
        # Returns domain: "<port>-<sandboxID>.<domain>"
    
    def get_url(self, port: int, protocol: str = "http") -> str
        # If CUBE_PROXY_NODE_IP set: returns "http://<IP>:<port_http>" with host header info
        # Otherwise: returns "http://<port>-<sandboxID>.<domain>"
    
    def connect_ws(self, port: int, path: str = "/", use_tls: bool = False):
        # Returns websocket connection, bypassing DNS if CUBE_PROXY_NODE_IP set
        # Use websockets library
    
    def http_get(self, port: int, path: str = "/", use_tls: bool = False) -> requests.Response
        # HTTP GET to sandbox service, bypassing DNS if CUBE_PROXY_NODE_IP set
    
    def kill(self) -> None
    def pause(self) -> None  
    def resume(self, timeout: int = 300) -> None
    def refresh(self, timeout: int = 300) -> None
    
    # context manager support
    def __enter__(self) -> "Sandbox"
    def __exit__(self, *args) -> None  # calls kill()
    
    # Properties
    @property
    def sandbox_id(self) -> str
    @property  
    def template_id(self) -> str
    @property
    def domain(self) -> str  # base domain e.g. "cube.app"
```

## Implementation Notes
1. For `CUBE_PROXY_NODE_IP` bypass, use `requests` with custom HTTPAdapter that overrides DNS resolution, OR simply use the IP directly and pass `Host` header.
2. For WebSocket with IP bypass, use `websockets` library with `host` override.
3. All methods should have proper type hints and docstrings.
4. Use `httpx` for async support optionally, but `requests` as primary sync client.
5. Error handling: parse CubeAPI error responses and raise appropriate exceptions.

## Files to write
Write ALL files. Start with config.py, exceptions.py, client.py, stream.py, sandbox.py, __init__.py, then examples and README.
