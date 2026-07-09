# Research: CubeSandbox Sandbox Template and Example Ecosystem

## Decision: Use Node.js Web Development as the First Increment

**Decision**: The first implementation slice should add a `node-web-sandbox` example with a buildable template, minimal web service, sandbox launch validation, README, Chinese README, and documentation registration.

**Rationale**: Existing examples already cover basic Python code execution, browser automation, network policy, route-aware egress, snapshot/rollback/clone, host mounts, benchmarking, and several agent integrations. A Node.js web development sandbox is a common runtime gap, is easy for new users to understand, and exercises template creation, exposed service behavior, environment configuration, and smoke validation without requiring platform changes.

**Alternatives considered**:

- Data science or code-interpreter template: already partially covered by the existing OpenAI Agents code interpreter example.
- Browser automation template: already covered by the Playwright browser sandbox.
- Advanced stateful or restricted-network example: valuable, but higher review cost for the first increment and already represented by snapshot and network examples.
- Multiple language templates at once: rejected because the issue explicitly supports incremental contributions and the minimum contribution bar is one buildable template plus one runnable example.

## Decision: Keep Examples Self-Contained Under `examples/<slug>/`

**Decision**: Each contribution should live in an `examples/<scenario-slug>/` directory with its own README, optional localized README, dependency files, source files, environment example, and validation script.

**Rationale**: Current examples follow this pattern and users already expect a self-contained README, dependencies, and runnable files per example. It also lets maintainers review one contribution independently.

**Alternatives considered**:

- Central shared example runner: rejected because it adds coordination overhead before the ecosystem has enough duplicate logic to justify it.
- Documentation-only catalog entries pointing to external repositories: rejected for the minimum bar because maintainers need reproducible local review.
- Placing templates under a new top-level `templates/` directory: rejected for the first slice because current user-facing examples already combine template creation and runnable flows.

## Decision: Build Templates Through the Existing OCI Image Flow

**Decision**: The first template should document `cubemastercli tpl create-from-image` using the example's image and required exposed ports, then use the returned template ID through the existing `CUBE_TEMPLATE_ID` environment variable convention.

**Rationale**: The current docs and examples already teach template-from-image and `CUBE_TEMPLATE_ID`. Reusing the same convention reduces onboarding cost and avoids new platform abstractions.

**Alternatives considered**:

- Prebuilt template ID only: rejected because it is not reproducible for contributors and reviewers.
- Hand-written internal template metadata: rejected because users are directed to the CLI/image flow in current docs.
- Adding a new template registry file: deferred until repeated examples prove the need for a machine-readable catalog.

## Decision: Use Existing Documentation Indexes for Discovery

**Decision**: Register the new example in both `docs/guide/tutorials/examples.md` and `docs/zh/guide/tutorials/examples.md`.

**Rationale**: Those pages are already the user-facing example catalog in English and Chinese. Updating both keeps discoverability consistent with the existing docs site.

**Alternatives considered**:

- Add a new ecosystem landing page: rejected for the first slice because it would duplicate the current example index.
- Register only English docs: rejected because the repository already maintains parallel Chinese pages for the same guide content.
- Rely on README discovery only: rejected because users should not need to inspect the repository tree manually.

## Decision: Treat Contracts as Review Standards

**Decision**: Planning contracts should define required contribution fields, README sections, catalog entry fields, and validation evidence. They should guide task generation and review without forcing a new runtime API.

**Rationale**: This feature primarily expands example and template assets. A contract here is a contributor and maintainer agreement, not a service endpoint.

**Alternatives considered**:

- OpenAPI or HTTP contracts: rejected because no new external service API is planned.
- JSON schema for a catalog file: deferred until the docs catalog becomes hard to maintain manually.
- Informal reviewer notes only: rejected because success criteria require consistent acceptance and reproducibility.

## Decision: Require Security and Resource Notes Per Entry

**Decision**: Every entry must document credentials, secret handling, external access, public access behavior, exposed ports, and resource recommendations.

**Rationale**: Templates and examples can accidentally normalize unsafe behavior. The contribution standard must preserve existing CubeSandbox controls for authentication, egress, public access, resource limits, and isolation.

**Alternatives considered**:

- Security review only for advanced examples: rejected because even simple web examples can expose public URLs or credentials.
- Central security warning only: rejected because each scenario has different ports, credentials, and network assumptions.

## Decision: Validation Evidence Must Be Repeatable, Not Just Claimed

**Decision**: Each entry should include commands and expected output that a reviewer can run, plus a brief validation evidence section or file describing environment, template build status, smoke-test result, and known limitations.

**Rationale**: The acceptance criteria require maintainers and other contributors to reproduce the result. Evidence shortens review and makes failures diagnosable.

**Alternatives considered**:

- Screenshots only: rejected because they are not enough to reproduce behavior.
- CI-only proof: rejected because local CubeSandbox deployments may vary and the issue explicitly expects local deployment validation.
- Free-form PR comments only: rejected because evidence should live near the example for future users.
