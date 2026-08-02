---
allowed-tools: Bash(./scripts/gh.sh pr review-comment:*),Bash(gh pr diff:*),Bash(gh pr view:*)
description: Review a pull request
---

Perform a comprehensive code review using subagents for key areas:

- code-quality-reviewer
- performance-reviewer
- test-coverage-reviewer
- documentation-accuracy-reviewer
- security-code-reviewer
- cube-project-reviewer

Instruct each to only provide noteworthy feedback. Once they finish, review the feedback and post only the feedback that you also deem noteworthy.

Provide feedback using inline comments for specific issues.
Use `./scripts/gh.sh pr review-comment <pr-number> --body-file -` with stdin for top-level comments.
Keep feedback concise.

The `cube-project-reviewer` agent enforces CubeSandbox-specific conventions and release gates: OCI multi-arch, bilingual README coverage, feature-change tests, unit-test gate, orphaned project wiring, and upgradability. It also covers terraform/k8s deployment design, Fix regression tests, workflows, CODEOWNERS, non-English characters, commit messages, and the close-policy reminder. Always include it.

---
