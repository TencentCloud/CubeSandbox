# Quickstart: Plan Validation for the Template and Example Ecosystem

This guide validates the planned first slice before `/speckit-tasks` and later implementation. It does not implement the example.

## Prerequisites

- A working local CubeSandbox deployment.
- `cubemastercli` configured for that deployment.
- Python 3.8+ available on the local machine.
- Ability to build or pull the example's OCI image.
- Access to `specs/001-sandbox-template-ecosystem/contracts/`.

## Validate the Plan Artifacts

1. Confirm all planning artifacts exist:

   ```bash
   test -f specs/001-sandbox-template-ecosystem/plan.md
   test -f specs/001-sandbox-template-ecosystem/research.md
   test -f specs/001-sandbox-template-ecosystem/data-model.md
   test -f specs/001-sandbox-template-ecosystem/quickstart.md
   test -f specs/001-sandbox-template-ecosystem/contracts/contribution-entry-contract.md
   test -f specs/001-sandbox-template-ecosystem/contracts/readme-contract.md
   test -f specs/001-sandbox-template-ecosystem/contracts/catalog-entry-contract.md
   test -f specs/001-sandbox-template-ecosystem/contracts/validation-evidence-contract.md
   ```

2. Confirm there are no unresolved planning placeholders:

   ```bash
   rg "NEEDS CLARIFICATION|ACTION REQUIRED|\\[FEATURE\\]|\\[DATE\\]|\\[###" \
     specs/001-sandbox-template-ecosystem \
     --glob '!quickstart.md' \
     --glob '!**/checklists/**'
   ```

   Expected result: no matches.

3. Confirm the first implementation slice is bounded:

   ```bash
   rg "node-web-sandbox|first implementation slice|one buildable template" specs/001-sandbox-template-ecosystem
   ```

   Expected result: the plan and research identify a single first example plus reusable contracts.

## Validate the Future Implementation Manually

After `/speckit-tasks` and implementation, reviewers should be able to run this flow for the planned first example:

1. Enter the example directory:

   ```bash
   cd examples/node-web-sandbox
   ```

2. Create the sandbox template from the documented image or local image build.

   Expected result: `cubemastercli` reports a template ID and the template reaches a ready state.

3. Configure local environment:

   ```bash
   cp .env.example .env
   ```

   Fill `E2B_API_URL`, `E2B_API_KEY`, and `CUBE_TEMPLATE_ID` with local values.

4. Install runner dependencies:

   ```bash
   pip install -r requirements.txt
   ```

5. Run the smoke validation:

   ```bash
   python validate.py
   ```

   Expected result: the script creates a sandbox from the template, starts or verifies the Node.js web service, receives the documented response, prints a success line, and cleans up the sandbox.

6. Review documentation registration:

   ```bash
   rg "Node.js|node-web-sandbox" docs/guide/tutorials/examples.md docs/zh/guide/tutorials/examples.md
   ```

   Expected result: both indexes include the new example with equivalent summaries.

7. Review required README content against [readme-contract.md](./contracts/readme-contract.md).

8. Review contribution evidence against [validation-evidence-contract.md](./contracts/validation-evidence-contract.md).

## Expected End State

- The first accepted contribution is independently runnable.
- Users can discover the entry from both examples indexes.
- Maintainers can review future entries against the same contracts.
- No platform authentication, egress, resource, or isolation behavior is weakened.
