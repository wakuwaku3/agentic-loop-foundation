---
name: submit-requirement
description: Convert a natural-language product goal, change request, bug report, or improvement to the agentic development loop itself into autonomous repository work. Use when the user states what they want built or changed, even when constraints or acceptance criteria are incomplete.
---

# Submit Requirement

Turn the user's short request into a verified, merged change without making them manage the engineering workflow.

1. Inspect the repository, its `AGENTS.md`, current behavior, and relevant external facts.
2. Infer safe, reversible details. Ask only when a missing decision has material cost, security, availability, data-loss, or product consequences.
3. State the interpreted objective, constraints, invariants, and observable completion criteria briefly; then proceed without waiting for confirmation when the interpretation is safe.
4. Sync the default branch and create a dedicated branch in a separate Git worktree. Never develop directly on the default branch.
5. Investigate, design, implement, test, and repair until every completion criterion and invariant is demonstrably satisfied.
6. Run the repository's full validation and secret guard. Do not bypass hooks or expose credentials in commands, logs, commits, or PR text.
7. Commit, push, open a PR, monitor required checks, address failures and review feedback, and merge the PR. Remove the worktree and branch after a successful merge.
8. Report the outcome, verification evidence, PR link, and any genuinely unresolved risk concisely.

Write GitHub Issue and PR titles, bodies, comments, and reviews in Japanese. Preserve code, logs, identifiers, proper nouns, and quotations in their necessary original form.

Treat requests about this workflow exactly like application requests. Improve the loop when doing so is required by the user's goal.
