# Tasks: CubeSandbox Sandbox Template and Example Ecosystem

**Input**: Design documents from `specs/001-sandbox-template-ecosystem/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md), [data-model.md](./data-model.md), [contracts/](./contracts/), [quickstart.md](./quickstart.md)

**Tests**: The feature specification does not request TDD. Tasks include executable smoke validation and documentation validation, but no separate failing-test-first tasks.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel because the task touches different files and has no dependency on another incomplete task.
- **[Story]**: Which user story the task belongs to. Setup, Foundational, and Polish tasks do not carry a story label.
- Every task includes an exact repository path.

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Create the planned example location and shared dependency/configuration files.

- [X] T001 Create the `examples/node-web-sandbox/` example directory.
- [X] T002 [P] Create placeholder environment configuration in `examples/node-web-sandbox/.env.example`.
- [X] T003 [P] Create Node.js project metadata and runnable scripts in `examples/node-web-sandbox/package.json`.
- [X] T004 [P] Create Python validation dependencies in `examples/node-web-sandbox/requirements.txt`.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Establish the common contribution standard and example boundaries before user-story work starts.

**Critical**: No user story work should begin until this phase is complete.

- [X] T005 Summarize the required contribution artifacts from `specs/001-sandbox-template-ecosystem/contracts/contribution-entry-contract.md` into `examples/node-web-sandbox/README.md`.
- [X] T006 Define the example's scenario metadata, required ports, resource guidance, security notes, and known limitations in `examples/node-web-sandbox/README.md`.
- [X] T007 Confirm the `node-web-sandbox` slug is distinct from existing examples and record the differentiation note in `examples/node-web-sandbox/README.md`.

**Checkpoint**: The example scope and acceptance standard are clear enough for implementation.

---

## Phase 3: User Story 1 - Reuse a Suitable Sandbox Starting Point (Priority: P1) MVP

**Goal**: A new user can find the Node.js web sandbox entry, build or obtain its template, run the minimum flow, and observe the documented result.

**Independent Test**: Follow only `examples/node-web-sandbox/README.md` and the docs index entry to create the template, configure environment variables, run `python validate.py`, and observe the expected success output.

### Implementation for User Story 1

- [X] T008 [P] [US1] Implement the Node.js HTTP service in `examples/node-web-sandbox/server.js`.
- [X] T009 [P] [US1] Implement the OCI image definition for Node.js 20, service startup, exposed port, and health behavior in `examples/node-web-sandbox/Dockerfile`.
- [X] T010 [P] [US1] Implement an in-sandbox HTTP smoke script in `examples/node-web-sandbox/smoke_test.py`.
- [X] T011 [US1] Implement the E2B-compatible sandbox creation and cleanup validator in `examples/node-web-sandbox/validate.py`.
- [X] T012 [US1] Complete the English runnable guide, expected output, resource guidance, security notes, limitations, troubleshooting, cleanup, and validation evidence sections in `examples/node-web-sandbox/README.md`.
- [X] T013 [P] [US1] Create the matching Chinese runnable guide with equivalent required safety and validation content in `examples/node-web-sandbox/README_zh.md`.
- [X] T014 [US1] Register the Node.js web sandbox entry in `docs/guide/tutorials/examples.md`.
- [X] T015 [US1] Register the matching Chinese Node.js web sandbox entry in `docs/zh/guide/tutorials/examples.md`.
- [X] T016 [US1] Run local syntax and dependency checks for `examples/node-web-sandbox/server.js`, `examples/node-web-sandbox/smoke_test.py`, and `examples/node-web-sandbox/validate.py`.

**Checkpoint**: User Story 1 is independently functional and demonstrates the minimum runnable ecosystem contribution.

---

## Phase 4: User Story 2 - Contribute an Incremental Template or Example (Priority: P2)

**Goal**: A contributor can use the Node.js web sandbox as a concrete pattern for adding one focused template/example with all required artifacts.

**Independent Test**: Compare the example directory against `specs/001-sandbox-template-ecosystem/contracts/contribution-entry-contract.md` and confirm every required artifact and metadata item is present.

### Implementation for User Story 2

- [X] T017 [P] [US2] Add a contributor-oriented artifact checklist to `examples/node-web-sandbox/README.md`.
- [X] T018 [P] [US2] Add the equivalent contributor-oriented artifact checklist to `examples/node-web-sandbox/README_zh.md`.
- [X] T019 [US2] Document the template build command, required `CUBE_TEMPLATE_ID` handoff, and validation command in `examples/node-web-sandbox/README.md`.
- [X] T020 [US2] Document the same template build, `CUBE_TEMPLATE_ID`, and validation flow in `examples/node-web-sandbox/README_zh.md`.
- [X] T021 [US2] Add a redacted validation evidence example covering build result, smoke-test result, cleanup, and limitations in `examples/node-web-sandbox/README.md`.
- [X] T022 [P] [US2] Add matching redacted validation evidence guidance in `examples/node-web-sandbox/README_zh.md`.

**Checkpoint**: A contributor can model a future single-entry contribution from this example without needing unrelated context.

---

## Phase 5: User Story 3 - Review Ecosystem Contributions Consistently (Priority: P3)

**Goal**: A maintainer can review the Node.js web sandbox and future entries against consistent acceptance criteria.

**Independent Test**: A maintainer can use the review checklist in the README and the planning contracts to decide whether the entry is accepted, needs revision, duplicate, or out of scope.

### Implementation for User Story 3

- [X] T023 [US3] Add a maintainer review checklist mapped to the contribution, README, catalog, and validation evidence contracts in `examples/node-web-sandbox/README.md`.
- [X] T024 [P] [US3] Add the equivalent maintainer review checklist in `examples/node-web-sandbox/README_zh.md`.
- [X] T025 [US3] Add duplicate-scenario and differentiation review guidance in `examples/node-web-sandbox/README.md`.
- [X] T026 [P] [US3] Add matching duplicate-scenario and differentiation review guidance in `examples/node-web-sandbox/README_zh.md`.
- [X] T027 [US3] Verify the English catalog entry follows the catalog-entry contract in `docs/guide/tutorials/examples.md`.
- [X] T028 [US3] Verify the Chinese catalog entry follows the catalog-entry contract in `docs/zh/guide/tutorials/examples.md`.

**Checkpoint**: Maintainers have a consistent local review standard for this and future ecosystem entries.

---

## Phase 6: User Story 4 - Demonstrate CubeSandbox Differentiated Capabilities (Priority: P4)

**Goal**: Advanced users can see how this first template fits CubeSandbox-specific capabilities and how future advanced examples should document those behaviors.

**Independent Test**: Read the Node.js web sandbox README and confirm it documents whether the example supports or intentionally excludes resumable work, stateful workspaces, coordinated services, and restricted network operation.

### Implementation for User Story 4

- [X] T029 [US4] Add a CubeSandbox capability notes section covering snapshot/resume, stateful workspace, multi-service scope, public access, and restricted-network behavior in `examples/node-web-sandbox/README.md`.
- [X] T030 [P] [US4] Add the equivalent CubeSandbox capability notes section in `examples/node-web-sandbox/README_zh.md`.
- [X] T031 [US4] Link the capability notes to existing relevant guides from `examples/node-web-sandbox/README.md`.
- [X] T032 [P] [US4] Link the capability notes to existing relevant Chinese guides from `examples/node-web-sandbox/README_zh.md`.

**Checkpoint**: The first entry sets expectations for advanced CubeSandbox behavior without claiming capabilities it does not demonstrate.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Validate formatting, reproducibility, security posture, and docs consistency across the completed slice.

- [X] T033 Run the plan artifact validation commands from `specs/001-sandbox-template-ecosystem/quickstart.md`.
- [X] T034 Run documentation link and content checks for `docs/guide/tutorials/examples.md`, `docs/zh/guide/tutorials/examples.md`, `examples/node-web-sandbox/README.md`, and `examples/node-web-sandbox/README_zh.md`.
- [X] T035 Check for committed secrets, private endpoints, or unredacted credentials in `examples/node-web-sandbox/.env.example`, `examples/node-web-sandbox/README.md`, and `examples/node-web-sandbox/README_zh.md`.
- [X] T036 Validate the final implementation flow from `examples/node-web-sandbox/README.md` in a supported local CubeSandbox deployment when one is available.
- [X] T037 Update validation evidence in `examples/node-web-sandbox/README.md` after local CubeSandbox validation.
- [X] T038 Update validation evidence in `examples/node-web-sandbox/README_zh.md` after local CubeSandbox validation.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies.
- **Foundational (Phase 2)**: Depends on Phase 1 and blocks all user-story phases.
- **US1 (Phase 3)**: Depends on Phase 2. This is the MVP and should be completed first.
- **US2 (Phase 4)**: Depends on Phase 2 and can proceed after US1 README structure exists.
- **US3 (Phase 5)**: Depends on Phase 2 and can proceed after US1 catalog/README content exists.
- **US4 (Phase 6)**: Depends on Phase 2 and can proceed after US1 README content exists.
- **Polish (Phase 7)**: Depends on all desired user stories being complete.

### User Story Dependencies

- **User Story 1 (P1)**: No dependency on other user stories after Phase 2. Delivers the MVP.
- **User Story 2 (P2)**: Builds on README files created in US1 but remains independently reviewable against the contribution contract.
- **User Story 3 (P3)**: Builds on README and catalog files created in US1 but can be reviewed independently against maintainer acceptance criteria.
- **User Story 4 (P4)**: Builds on README files created in US1 and adds advanced capability expectations.

### Parallel Opportunities

- T002, T003, and T004 can run in parallel after T001.
- T008, T009, and T010 can run in parallel after Phase 2.
- T013 can run in parallel with T012 once the English README outline is stable.
- T017 and T018 can run in parallel.
- T021 and T022 can run in parallel after T019 and T020 establish the flow.
- T023 and T024 can run in parallel.
- T025 and T026 can run in parallel.
- T029 and T030 can run in parallel.
- T031 and T032 can run in parallel after capability notes exist.

---

## Parallel Example: User Story 1

```bash
Task: "T008 [P] [US1] Implement the Node.js HTTP service in examples/node-web-sandbox/server.js"
Task: "T009 [P] [US1] Implement the OCI image definition for Node.js 20, service startup, exposed port, and health behavior in examples/node-web-sandbox/Dockerfile"
Task: "T010 [P] [US1] Implement an in-sandbox HTTP smoke script in examples/node-web-sandbox/smoke_test.py"
```

## Parallel Example: User Story 2

```bash
Task: "T017 [P] [US2] Add a contributor-oriented artifact checklist to examples/node-web-sandbox/README.md"
Task: "T018 [P] [US2] Add the equivalent contributor-oriented artifact checklist to examples/node-web-sandbox/README_zh.md"
```

## Parallel Example: User Story 3

```bash
Task: "T023 [US3] Add a maintainer review checklist mapped to the contribution, README, catalog, and validation evidence contracts in examples/node-web-sandbox/README.md"
Task: "T024 [P] [US3] Add the equivalent maintainer review checklist in examples/node-web-sandbox/README_zh.md"
```

## Parallel Example: User Story 4

```bash
Task: "T029 [US4] Add a CubeSandbox capability notes section covering snapshot/resume, stateful workspace, multi-service scope, public access, and restricted-network behavior in examples/node-web-sandbox/README.md"
Task: "T030 [P] [US4] Add the equivalent CubeSandbox capability notes section in examples/node-web-sandbox/README_zh.md"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup.
2. Complete Phase 2: Foundational.
3. Complete Phase 3: User Story 1.
4. Stop and validate that the Node.js web sandbox can be discovered, built, run, and cleaned up using only its README and docs entry.

### Incremental Delivery

1. Deliver US1 as the mergeable minimum: one buildable template, one runnable example, README/README_zh, docs registration, and validation flow.
2. Add US2 to make the contribution pattern clear for future ecosystem participants.
3. Add US3 to make maintainer review consistent.
4. Add US4 to document CubeSandbox-specific advanced behavior expectations without expanding the first example beyond its scope.

### Parallel Team Strategy

1. One contributor creates the example runtime files.
2. One contributor writes English and Chinese docs in parallel after the outline is stable.
3. One maintainer reviews the example against the four contracts and feeds revisions back into the README and catalog entries.

---

## Notes

- Keep the first implementation slice limited to `examples/node-web-sandbox/` plus the two examples index files.
- Do not add real credentials or private endpoints to `.env.example` or README files.
- Use existing environment variable names where possible: `E2B_API_URL`, `E2B_API_KEY`, and `CUBE_TEMPLATE_ID`.
- Keep future language/runtime expansion out of this task list unless it is documented as follow-up guidance.
