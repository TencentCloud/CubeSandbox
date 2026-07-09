# Implementation Plan: WebUI Terminal Login

**Branch**: `feature/webui-terminal-login` | **Date**: 2026-07-08 | **Spec**: [spec.md](./spec.md)

## Summary

Implement a WebUI terminal by adding a CubeAPI WebSocket bridge from browser clients to the existing EnvD PTY process API, then adding an xterm-based React terminal panel in sandbox list/detail views. The backend validates auth and sandbox running state, proxies PTY start/input/output/resize/kill messages, enforces session timeout, and writes audit logs.

## Technical Context

**Language/Version**: Rust 2021 for CubeAPI; TypeScript/React for WebUI.

**Primary Dependencies**: CubeAPI uses Axum 0.7 with WebSocket feature, Tokio, reqwest, serde_json, tracing. WebUI should use `@xterm/xterm` and `@xterm/addon-fit`.

**Storage**: No persistent storage. Terminal session state is in memory for each WebSocket connection; audit events use structured logging.

**Testing**: CubeAPI `cargo test`; WebUI `npm run lint`, plus component/unit tests if the project test runner is added. Initial frontend validation can be TypeScript build plus manual browser validation if no test runner exists.

**Target Platform**: CubeAPI Linux server behind existing HTTPS/WSS deployment path; WebUI browser app.

**Project Type**: Full-stack web application.

**Constraints**: Must reuse EnvD PTY/process API; must not bypass auth, sandbox state checks, or sandbox security policies; terminal delivery must be isolated per WebSocket session.

## Constitution Check

The constitution is a placeholder and has no enforceable gates. Project-specific gates:

- Reuse existing EnvD PTY/process API.
- Keep WebSocket terminal route behind existing auth middleware.
- Add audit logs and no secret logging.
- Keep UI text in i18n resources.
- Verify backend and frontend compile/test targets.

## Project Structure

```text
CubeAPI/
├── Cargo.toml
├── src/
│   ├── handlers/terminal.rs
│   ├── handlers/mod.rs
│   ├── models/mod.rs
│   ├── routes.rs
│   └── services/
│       ├── mod.rs
│       └── terminal.rs

web/
├── package.json
├── src/
│   ├── api/client.ts
│   ├── components/TerminalPanel.tsx
│   ├── pages/Sandboxes.tsx
│   ├── pages/SandboxDetail.tsx
│   └── locales/
│       ├── en/terminal.json
│       └── zh/terminal.json

docs/guide/webui-terminal.md
specs/002-webui-terminal-login/
└── tasks.md
```

## Research Decisions

- Bridge browser WebSocket messages to EnvD Connect-JSON PTY endpoints: `process.Process/Start`, `SendInput`, `Update`, and `SendSignal`.
- Use JSON WebSocket control frames: browser sends `input`, `resize`, `close`; server sends `output`, `status`, `error`, `exit`.
- Start `/bin/bash -i -l` with `TERM=xterm-256color` and UTF-8 locale.
- Derive EnvD host as `49983-{sandbox_id}.{sandbox_domain}` from sandbox detail.
- Use existing CubeAPI auth middleware for the WebSocket route and forward `Authorization`/`X-API-Key` through normal callback checks.
- Initial container selector defaults to the platform default container until CubeAPI exposes container metadata.

## Phase 1 Design

- Backend adds terminal models and service for Connect-JSON envelope encoding/decoding.
- Backend handler upgrades authenticated WebSocket and drives bidirectional IO tasks.
- Frontend adds terminal modal/panel and wires terminal WebSocket URL creation with existing auth tokens.
- Docs describe local validation, HTTPS/WSS, auth, state checks, and limitations.
