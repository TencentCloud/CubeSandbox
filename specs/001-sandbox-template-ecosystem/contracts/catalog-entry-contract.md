# Contract: Documentation Catalog Entry

This contract defines the minimum user-facing documentation registration for an example.

## Required Fields

- `display_name`: Link text users can scan quickly.
- `example_link`: Link to the example directory in the repository.
- `summary`: One sentence describing the scenario, runtime, and demonstrated CubeSandbox behavior.
- `category`: Primary user scenario category.

## Required Locations

- `docs/guide/tutorials/examples.md`
- `docs/zh/guide/tutorials/examples.md`

## Acceptance Rules

- The English and Chinese summaries describe the same capability.
- The link points to the correct repository path.
- The summary is distinct from existing entries.
- The entry helps users decide whether the example matches their need within 5 minutes.
- The entry follows the table style already used by the examples page.

## Duplicate Handling

If a new contribution overlaps an existing example, its catalog summary must state the differentiating value. If no differentiating value exists, the contribution should revise the existing example instead of adding a new entry.
