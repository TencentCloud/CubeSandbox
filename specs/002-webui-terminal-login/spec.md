# Feature Specification: WebUI Terminal Login

**Feature Branch**: `feature/webui-terminal-login`

**Created**: 2026-07-08

**Status**: Draft

**Input**: User description: "在 WebUI 沙箱/容器实例列表或详情页提供打开终端入口；使用成熟 Web 终端组件；后端通过 WebSocket 双向打通浏览器和实例 TTY，复用现有执行/附着能力；支持会话管理、鉴权、运行态校验、多容器选择、安全、国际化、文档和测试。"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Open Terminal From WebUI (Priority: P1)

As an authorized WebUI user, I want to open an interactive terminal from a running sandbox list row or detail page so I can execute commands inside the sandbox without leaving the dashboard.

**Why this priority**: This is the core user-visible workflow and proves end-to-end terminal access.

**Independent Test**: Start a running sandbox, click "Open terminal" in WebUI, run `ls`, and verify input/output, ANSI display, scrollback, paste, and resize work.

**Acceptance Scenarios**:

1. **Given** a sandbox is running, **When** the user opens the terminal, **Then** an interactive terminal panel connects to that sandbox.
2. **Given** a sandbox is paused or otherwise not login-ready, **When** the user views the list or detail page, **Then** the terminal entry is disabled with a clear reason.
3. **Given** the terminal is connected, **When** the user types shell commands, **Then** stdout/stderr and ANSI terminal output are displayed correctly.

---

### User Story 2 - Secure WebSocket TTY Bridge (Priority: P2)

As a platform operator, I want CubeAPI to expose an authenticated WebSocket terminal bridge that reuses the existing sandbox process/PTY API so terminal access follows the same permissions and isolation boundaries as existing command execution.

**Why this priority**: The WebUI cannot be production-safe without backend auth, state validation, TTY resize, and audit.

**Independent Test**: Connect to the WebSocket with valid credentials and a running sandbox, exchange terminal input/output and resize messages, then verify unauthorized and paused-sandbox connections are rejected.

**Acceptance Scenarios**:

1. **Given** a user lacks valid auth credentials, **When** they attempt terminal WebSocket connection, **Then** the connection is rejected.
2. **Given** the target sandbox is not running, **When** a terminal connection is attempted, **Then** the connection is rejected with a clear message.
3. **Given** a terminal session starts, **When** the browser sends resize messages, **Then** the backend synchronizes the TTY size.
4. **Given** a terminal session starts, **When** it is opened or closed, **Then** CubeAPI records audit logs including actor, time, target sandbox, and optional container.

---

### User Story 3 - Session Lifecycle and Multi-Session Use (Priority: P3)

As a user, I want terminal sessions to remain stable, support intentional disconnects, idle timeout, reconnect messaging, and multiple concurrent sessions so different users or windows can work without interfering.

**Why this priority**: Stable session behavior is required for realistic operational use and concurrent WebUI workflows.

**Independent Test**: Open two terminal sessions to the same or different running sandboxes, verify both can execute commands independently, disconnect one, and verify the other remains active.

**Acceptance Scenarios**:

1. **Given** two terminals connect to the same sandbox, **When** each user types commands, **Then** each session receives only its own PTY output.
2. **Given** a session is idle beyond the configured timeout, **When** timeout occurs, **Then** the server closes the session and the UI shows a clear reconnect prompt.
3. **Given** the network connection drops, **When** the WebSocket closes unexpectedly, **Then** the UI presents a disconnected status and allows opening a new session.

---

### User Story 4 - Documentation and Local Validation (Priority: P4)

As a contributor or adopter, I want documentation describing terminal login setup, permissions, limitations, and verification steps so I can validate the feature locally within 30 minutes.

**Why this priority**: Documentation and validation steps make the feature adoptable and reviewable.

**Independent Test**: Follow the documentation from a local deployment to open a terminal in WebUI and run commands in a container within 30 minutes.

**Acceptance Scenarios**:

1. **Given** a local CubeSandbox deployment, **When** a contributor follows the docs, **Then** they can open a terminal and execute commands.
2. **Given** an operator reviews docs, **When** they read terminal limitations and permissions, **Then** they understand auth requirements, HTTPS/WSS expectations, and known constraints.

### Edge Cases

- Sandbox ID does not exist.
- Sandbox exists but is paused, pausing, deleted, or otherwise not running.
- Browser WebSocket connects without required auth credentials.
- EnvD process API rejects terminal creation.
- WebSocket closes during terminal startup.
- Terminal output contains binary bytes or ANSI control sequences.
- User resizes the terminal rapidly.
- Multiple sessions connect to the same sandbox concurrently.
- Session is idle until timeout.
- A sandbox has multiple containers and the user chooses a target container; v1 must expose the default container and leave the UI/API shape ready for explicit container IDs.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: WebUI MUST show an "Open terminal" entry on sandbox list and detail views for running sandboxes.
- **FR-002**: WebUI MUST disable the terminal entry for non-running sandboxes and show a clear reason.
- **FR-003**: WebUI MUST use a mature terminal component that supports interactive input/output, ANSI colors, cursor control, resize, paste, and scrollback.
- **FR-004**: CubeAPI MUST expose an authenticated WebSocket terminal channel for browser clients.
- **FR-005**: CubeAPI MUST validate that the target sandbox exists and is running before starting a terminal session.
- **FR-006**: CubeAPI MUST bridge WebSocket input/output to the existing EnvD PTY/process API rather than introducing a separate execution mechanism.
- **FR-007**: The terminal channel MUST support TTY mode and window-size synchronization.
- **FR-008**: Terminal sessions MUST support active disconnect, abnormal disconnect messages, idle timeout, and multiple concurrent sessions.
- **FR-009**: CubeAPI MUST record audit logs for terminal session open and close events, including actor, timestamp, sandbox ID, and container ID when available.
- **FR-010**: The feature MUST preserve existing sandbox permission and network policy boundaries.
- **FR-011**: UI text MUST be added to the existing English and Chinese localization resources.
- **FR-012**: Documentation MUST describe usage, auth requirements, HTTPS/WSS expectations, limitations, and local validation steps.
- **FR-013**: Tests MUST cover backend session establishment, auth rejection, state rejection, disconnect, resize, and frontend terminal component behavior.

### Key Entities

- **Terminal Session**: A WebSocket-backed interactive PTY connection to one sandbox/container.
- **Terminal Message**: A browser/server message representing input, output, resize, status, error, or close.
- **Terminal Target**: The sandbox and optional container selected for login.
- **Audit Event**: Structured record of terminal session open/close outcomes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A running sandbox can be opened from WebUI terminal and execute `ls` with visible output.
- **SC-002**: ANSI-colored output and cursor-oriented commands render correctly in the terminal component.
- **SC-003**: Terminal resize messages reach the backend and update the PTY dimensions.
- **SC-004**: Unauthorized terminal WebSocket attempts are rejected.
- **SC-005**: Non-running sandbox terminal attempts are rejected or disabled before connection.
- **SC-006**: Two concurrent sessions can operate without cross-output interference.
- **SC-007**: Session open/close audit logs include actor, timestamp, sandbox ID, and session ID.
- **SC-008**: Contributors can follow docs to validate local terminal login within 30 minutes.

## Assumptions

- CubeAPI is the correct control-plane backend for the WebUI terminal WebSocket.
- The existing EnvD process PTY API exposed on sandbox port 49983 is the execution mechanism to reuse.
- Initial multi-container support may expose default container behavior while preserving API/UI fields for container selection when container metadata becomes available.
- WSS is provided by the existing deployment TLS/proxy layer; CubeAPI implements WebSocket handling independent of certificate termination.
