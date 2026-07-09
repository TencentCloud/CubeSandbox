# Data Model: CubeSandbox Sandbox Template and Example Ecosystem

## Template Entry

Represents one reusable sandbox starting point.

**Fields**

- `slug`: Stable kebab-case identifier, unique under `examples/`.
- `title`: Human-readable name.
- `scenario_category`: User-oriented category such as language runtime, web development, data science, browser automation, code interpreter, agent framework, stateful workspace, multi-service, or restricted network.
- `template_source`: Image or template definition source used to create the sandbox template.
- `build_or_acquisition_steps`: Ordered steps to create or obtain the template.
- `required_ports`: Ports that must be exposed for the minimum example.
- `resource_recommendation`: Minimum practical CPU, memory, writable layer, timeout, and any workload-specific notes.
- `security_notes`: Authentication, secret handling, external access, public access, and isolation assumptions.
- `known_limitations`: Unsupported environments, missing features, or expected failure modes.
- `status`: Planned, ready for review, accepted, needs revision, deprecated.

**Validation Rules**

- `slug` must be unique and match the example directory name.
- `template_source`, build or acquisition steps, resource recommendation, security notes, and known limitations are required for accepted entries.
- Accepted entries must have a linked example flow and documentation registration.

**Relationships**

- Has one or more example flows.
- Has one documentation registration in each required docs locale.
- Has one or more validation evidence records.

## Example Flow

Represents the minimum runnable workflow proving the template works.

**Fields**

- `entry_slug`: Template entry slug.
- `purpose`: What the flow demonstrates.
- `prerequisites`: Local deployment, CLI, SDK, credentials, template ID, and environment requirements.
- `setup_steps`: Dependency installation and environment setup steps.
- `run_steps`: Commands or actions that execute the example.
- `expected_results`: Observable output, service response, file, or sandbox state that proves success.
- `cleanup_steps`: How to stop or remove resources created by the flow.
- `failure_notes`: Common failures and where to look for troubleshooting.

**Validation Rules**

- Accepted flows must be runnable from the README without hidden setup.
- Expected results must be concrete enough for reviewer comparison.
- Cleanup behavior must be documented for sandbox resources.

**Relationships**

- Belongs to one template entry.
- Produces validation evidence.

## Documentation Registration

Represents the user-facing catalog entry that makes an example discoverable.

**Fields**

- `entry_slug`: Template entry slug.
- `doc_path`: Documentation index path.
- `localized_doc_path`: Localized documentation path when maintained by the project.
- `display_name`: User-facing link label.
- `summary`: One-sentence description of the scenario and demonstrated value.
- `category`: Scenario category used for scanning.

**Validation Rules**

- Registration must link to the example directory.
- Summary must mention the user-facing scenario and the main capability demonstrated.
- English and Chinese docs should remain equivalent when both indexes exist.

**Relationships**

- Belongs to one template entry.

## Contribution Evidence

Represents reviewer-verifiable proof that the example was validated.

**Fields**

- `entry_slug`: Template entry slug.
- `validated_by`: Contributor or maintainer identifier.
- `validated_on`: Date of validation.
- `environment`: Deployment type and relevant host/runtime details.
- `template_build_result`: Template ID or build status summary.
- `example_run_result`: Smoke-test command and observed result.
- `resource_observations`: Notes about runtime, memory, writable layer, timeout, or other resource behavior.
- `network_observations`: Notes about outbound access, public access, and blocked or allowed traffic.
- `limitations_found`: Any caveats discovered during validation.

**Validation Rules**

- Accepted entries must include evidence sufficient for a reviewer to repeat validation.
- Evidence must not contain secrets, private endpoints that should not be published, or sensitive logs.

**Relationships**

- Belongs to one template entry.
- References one or more example flows.

## Scenario Category

Represents a searchable grouping for examples.

**Fields**

- `name`: Category name.
- `description`: What users should expect from entries in the category.
- `differentiators`: Optional CubeSandbox capabilities highlighted by the category.

**Validation Rules**

- Category names should be user-oriented and avoid duplicating existing categories unless the entry genuinely fits.
- Entries can mention multiple differentiators, but should have one primary category for scanning.

## Review Decision

Represents the maintainer outcome for a contribution.

**Fields**

- `entry_slug`: Template entry slug.
- `decision`: Accepted, needs revision, duplicate, out of scope, or deprecated.
- `review_notes`: Specific findings tied to the contribution contract.
- `blocking_items`: Required fixes before acceptance.
- `reviewed_on`: Review date.

**State Transitions**

```text
planned -> ready for review -> accepted
planned -> ready for review -> needs revision -> ready for review
planned -> ready for review -> duplicate
planned -> ready for review -> out of scope
accepted -> deprecated
```

**Validation Rules**

- Needs-revision decisions must identify missing contract items.
- Duplicate decisions must reference the overlapping entry or scenario.
