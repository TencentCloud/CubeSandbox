# Tasks: WebUI Terminal Login

**Input**: Design documents from `specs/002-webui-terminal-login/`

**Tests**: Included because the specification explicitly requires backend and frontend tests.

## Phase 1: Setup

- [X] T001 Add WebUI terminal dependencies `@xterm/xterm` and `@xterm/addon-fit` in `web/package.json`
- [X] T002 Add backend terminal module declarations in `CubeAPI/src/handlers/mod.rs` and `CubeAPI/src/services/mod.rs`
- [X] T003 [P] Create WebUI terminal locale files in `web/src/locales/en/terminal.json` and `web/src/locales/zh/terminal.json`

## Phase 2: Backend Foundation

- [X] T004 Define terminal WebSocket message models in `CubeAPI/src/models/mod.rs`
- [X] T005 Implement Connect-JSON envelope encode/decode helpers in `CubeAPI/src/services/terminal.rs`
- [X] T006 [P] Add unit tests for Connect-JSON envelope helpers in `CubeAPI/src/services/terminal.rs`
- [X] T007 Implement EnvD PTY URL/header/request construction in `CubeAPI/src/services/terminal.rs`
- [X] T008 [P] Add unit tests for terminal target validation and EnvD request construction in `CubeAPI/src/services/terminal.rs`

## Phase 3: User Story 1 - Open Terminal From WebUI (Priority: P1)

- [X] T009 [US1] Implement terminal WebSocket upgrade handler skeleton in `CubeAPI/src/handlers/terminal.rs`
- [X] T010 [US1] Register terminal WebSocket routes for root and `/cubeapi/v1` surfaces in `CubeAPI/src/routes.rs`
- [X] T011 [US1] Implement xterm-based terminal panel component in `web/src/components/TerminalPanel.tsx`
- [X] T012 [US1] Add running-state terminal entry in sandbox list rows in `web/src/pages/Sandboxes.tsx`
- [X] T013 [US1] Add running-state terminal entry in sandbox detail header in `web/src/pages/SandboxDetail.tsx`
- [X] T014 [US1] Add terminal namespace to i18n resource registration in `web/src/i18n/resources.ts`

## Phase 4: User Story 2 - Secure WebSocket TTY Bridge (Priority: P2)

- [X] T015 [US2] Validate sandbox existence and running state before PTY start in `CubeAPI/src/handlers/terminal.rs`
- [X] T016 [US2] Implement EnvD PTY start stream and output forwarding in `CubeAPI/src/services/terminal.rs`
- [X] T017 [US2] Implement browser input forwarding to EnvD `SendInput` in `CubeAPI/src/services/terminal.rs`
- [X] T018 [US2] Implement resize forwarding to EnvD `Update` in `CubeAPI/src/services/terminal.rs`
- [X] T019 [US2] Add terminal session audit logs for open and close in `CubeAPI/src/handlers/terminal.rs`
- [X] T020 [P] [US2] Add backend tests for auth/state rejection and resize handling in `CubeAPI/src/handlers/terminal.rs`

## Phase 5: User Story 3 - Session Lifecycle and Multi-Session Use (Priority: P3)

- [X] T021 [US3] Add session IDs, status messages, and close/error protocol handling in `CubeAPI/src/handlers/terminal.rs`
- [X] T022 [US3] Implement idle timeout and active disconnect cleanup in `CubeAPI/src/handlers/terminal.rs`
- [X] T023 [US3] Implement terminal reconnect/disconnected UI states in `web/src/components/TerminalPanel.tsx`
- [X] T024 [P] [US3] Add backend tests for concurrent session isolation and idle timeout in `CubeAPI/src/handlers/terminal.rs`

## Phase 6: User Story 4 - Documentation and Local Validation (Priority: P4)

- [X] T025 [US4] Write WebUI terminal usage documentation in `docs/guide/webui-terminal.md`
- [X] T026 [US4] Add documentation navigation link in `docs/.vitepress/config.mjs`
- [X] T027 [US4] Add known limitations and multi-container notes in `docs/guide/webui-terminal.md`

## Phase 7: Validation and Polish

- [X] T028 Run backend formatting with `cargo fmt --manifest-path CubeAPI/Cargo.toml`
- [X] T029 Run backend terminal tests with `cargo test terminal --manifest-path CubeAPI/Cargo.toml`
- [X] T030 Run WebUI test and type/build validation with `npm run test --prefix web` and `npm run build --prefix web`
- [X] T031 Manually validate local WebUI terminal flow from `docs/guide/webui-terminal.md`

## Dependencies & Execution Order

- Phase 1 before backend/frontend implementation.
- Phase 2 before the WebSocket bridge.
- US1 can validate the visible entry and modal shell.
- US2 completes secure PTY IO and audit.
- US3 adds lifecycle hardening.
- US4 documents the validated behavior.

## MVP Scope

Complete Phases 1-3 and enough of Phase 4 to open a PTY, send input, receive output, and resize for a running sandbox.
