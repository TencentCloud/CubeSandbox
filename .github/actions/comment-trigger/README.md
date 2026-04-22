# comment-trigger

A reusable composite action that gates a workflow job on a chat-style
comment command (e.g. `@codebuddy review`) and resolves the underlying
Pull Request or Issue context in a single step.

It is intended to be the single source of truth for:

- deciding *whether* a run should proceed,
- deciding *who* is allowed to trigger the run,
- and returning the PR/Issue metadata downstream steps need (number,
  SHAs, base / head / merge refs, title, body, author, etc.).

Calling workflows only need to declare the right `on:` triggers and
then gate every real step on `steps.<id>.outputs.triggered == 'true'`.

---

## Supported events

| Event                        | When `triggered` is true                                                                                         |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `pull_request`               | always (caller already scopes by `types:` in `on:`)                                                              |
| `pull_request_target`        | always                                                                                                           |
| `issues`                     | always                                                                                                           |
| `issue_comment`              | comment body contains `command` (case-insensitive) **and** commenter matches `allowed-associations` / `allowed-users` |

Any other event is unsupported and will produce `triggered=false` with a
warning.

> ⚠️ **GitHub behaviour gotcha.** `issue_comment` events always execute
> the workflow file from the repository's **default branch**. Edits to
> this workflow / action made on a feature branch will NOT take effect
> for comment triggering until they are merged into the default branch.
> Test `pull_request` triggers on feature branches, but test
> `issue_comment` triggers only after merging.

---

## Inputs

| Name                   | Required | Default                          | Description                                                                                                          |
| ---------------------- | -------- | -------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `command`              | no       | `''`                             | Text to look for in comment bodies (case-insensitive). Leave empty to always treat comment events as triggered.      |
| `allowed-associations` | no       | `OWNER,MEMBER,COLLABORATOR`      | Comma-separated [`author_association`] values allowed to trigger via comment. Empty string disables this check.      |
| `allowed-users`        | no       | `''`                             | Optional comma-separated GitHub logins. When set, the commenter must match one of these (case-insensitive).          |
| `reaction`             | no       | `eyes`                           | Reaction to apply to the triggering comment. Valid: `+1`, `-1`, `laugh`, `confused`, `heart`, `hooray`, `rocket`, `eyes`. Empty disables. |
| `github-token`         | no       | `${{ github.token }}`            | Token for API calls (reactions + resolving PR details).                                                              |

`allowed-associations` AND `allowed-users` are ANDed together: both
checks must pass. Leave either empty to disable that particular check.

[`author_association`]: https://docs.github.com/en/graphql/reference/enums#commentauthorassociation

---

## Outputs

| Output         | Description                                                                 |
| -------------- | --------------------------------------------------------------------------- |
| `triggered`    | `'true'` when the job should proceed, `'false'` otherwise. The primary gate. |
| `kind`         | `'pull_request'` or `'issue'`                                               |
| `number`       | PR or Issue number (stringified)                                            |
| `title`        | PR or Issue title                                                           |
| `body`         | PR or Issue body                                                            |
| `author`       | PR / Issue author login                                                     |
| `base_ref`     | (PR only) base ref name                                                     |
| `base_sha`     | (PR only) base commit SHA                                                   |
| `head_ref`     | (PR only) head ref name                                                     |
| `head_sha`     | (PR only) head commit SHA                                                   |
| `merge_ref`    | (PR only) `refs/pull/<number>/merge` — pass to `actions/checkout`           |
| `comment_id`   | Triggering comment ID (empty for non-comment events)                        |
| `comment_body` | Triggering comment body                                                     |
| `commenter`    | Triggering commenter login                                                  |

---

## Side effects

When `triggered` becomes true and the event is `issue_comment`, the
action applies `reaction` to the triggering comment (default 👀) so the
user gets immediate feedback that the run was accepted. This requires
the calling workflow to have `issues: write` (for issue comments) or
`pull-requests: write` (for PR comments).

---

## Usage

### 1. PR review triggered by `pull_request` OR `@codebuddy review` comment

```yaml
name: Review pull requests
on:
  pull_request:
    types: [opened]
  issue_comment:
    types: [created]

jobs:
  review:
    runs-on: ubuntu-latest
    # Cheap gate: for issue_comment events, require it to be on a PR.
    # Command matching and author-association checks are delegated to
    # the comment-trigger action below.
    if: github.event_name != 'issue_comment' || github.event.issue.pull_request != null
    permissions:
      contents: read
      pull-requests: write
      issues: write
    steps:
      - name: Checkout workflow definitions
        uses: actions/checkout@v5
        with:
          sparse-checkout: .github
          sparse-checkout-cone-mode: false

      - name: Resolve trigger
        id: trigger
        uses: ./.github/actions/comment-trigger
        with:
          command: '@codebuddy review'

      - name: Checkout PR merge commit
        if: steps.trigger.outputs.triggered == 'true'
        uses: actions/checkout@v5
        with:
          ref: ${{ steps.trigger.outputs.merge_ref }}

      # ... all subsequent "real work" steps guarded with the same if
```

### 2. Issue triage triggered by new issues OR `@triage go` comment

```yaml
name: Triage backend issues
on:
  issues:
    types: [opened]
  issue_comment:
    types: [created]

jobs:
  triage:
    runs-on: ubuntu-latest
    # Cheap gate: for issue_comment events, require it to be on an Issue
    # (not a PR) — this workflow doesn't handle PRs.
    if: github.event_name != 'issue_comment' || github.event.issue.pull_request == null
    permissions:
      contents: read
      issues: write
    steps:
      - uses: actions/checkout@v5
        with:
          sparse-checkout: .github
          sparse-checkout-cone-mode: false

      - id: trigger
        uses: ./.github/actions/comment-trigger
        with:
          command: '@triage go'
          allowed-associations: 'OWNER,MEMBER'
          reaction: 'rocket'

      - name: Run backend triage
        if: steps.trigger.outputs.triggered == 'true' && steps.trigger.outputs.kind == 'issue'
        env:
          ISSUE_NUMBER: ${{ steps.trigger.outputs.number }}
          ISSUE_TITLE:  ${{ steps.trigger.outputs.title }}
          ISSUE_BODY:   ${{ steps.trigger.outputs.body }}
          ISSUE_AUTHOR: ${{ steps.trigger.outputs.author }}
        run: ./scripts/backend-triage.sh
```

### 3. Restrict to a specific user list instead of associations

```yaml
      - id: trigger
        uses: ./.github/actions/comment-trigger
        with:
          command: '@deploy staging'
          allowed-associations: ''                # disable association check
          allowed-users: 'alice,bob,release-bot'  # only these users may trigger
```

---

## Design notes

- **Why a composite action and not a reusable workflow?**
  Composite actions can be invoked as a single step and produce outputs
  usable by later steps in the same job. Reusable workflows run in a
  separate job and cannot directly feed per-step `if:` gates. Since the
  core job needs `triggered` to selectively run, composite is the
  better fit.
- **Why not put the keyword in the workflow's job-level `if:`?**
  Duplicating the keyword at the workflow level and inside the action
  makes it too easy to change one and forget the other. The action is
  the single source of truth for "is this comment a real command?".
  The workflow-level `if:` is kept intentionally cheap and generic,
  only filtering events that could never be handled (e.g. Issue
  comments for a PR-only workflow).
- **Why `issue_comment` instead of `pull_request_review_comment`?**
  `issue_comment` fires for top-level comments on both PRs and Issues,
  which matches natural user behaviour (people leave chat commands as
  top-level comments, not as inline review comments).
- **Security on fork PRs.**
  `pull_request` events from forks run with a read-only token and no
  secrets, so the review path will fail silently if it needs to write
  back to GitHub. For that case, switch the caller to
  `pull_request_target` and be aware that the PR author's code then
  runs in a privileged context — only do this if the workflow does
  NOT execute PR-supplied code (this action only reads PR metadata
  via the GitHub API, so it is safe). Author-association still gates
  comment triggers because the `author_association` of a fork PR
  contributor is `CONTRIBUTOR` or `NONE`, not `MEMBER` / `OWNER` /
  `COLLABORATOR`.

---

## Maintenance

- The gating logic lives entirely inside
  [`action.yml`](./action.yml)'s `github-script` step. To add a new
  supported event (e.g. `discussion_comment`), extend the `if / else if`
  chain there and populate the same output schema.
- Keep the output schema backward-compatible: callers rely on every
  field listed in the Outputs table above. If you need to add a new
  field, add it; do not rename or remove existing ones without
  coordinating with every caller.
- Do NOT add secrets to this action's inputs. The only token it needs
  (`github-token`) already defaults to the workflow-provided token,
  which carries exactly the permissions declared in the caller's
  `permissions:` block.
