---
name: submit-requirement
description: Convert a natural-language product goal, change request, bug report, or improvement to the agentic development loop itself into autonomous repository work. Use when the user states what they want built or changed, even when constraints or acceptance criteria are incomplete.
---

# Submit Requirement

Turn the user's short request into a verified, merged change without making them manage the engineering workflow.

## Queue-first intake

For an ordinary build or change request received in an interactive session, use the repository Issue queue instead of starting implementation when the queue is configured and its Supervisor is healthy:

1. Exclude read-only questions, diagnosis, status checks, and operational commands such as `start` and `stop`. Also skip intake when the user explicitly requests synchronous or direct implementation.
2. Verify `.agentic-loop.toml`, executable `bin/agentic-loop`, a `running` first line from `bin/agentic-loop status`, and GitHub Issue read/write access for the repository.
3. Search open Issue titles and bodies before creating anything. Inspect the body and comments of plausible matches and reuse an Issue that asks for the same user-visible outcome.
4. Classify the requirement with exactly one category label: `category:loop-continuity`, `category:confidentiality-incident`, `category:integrity-incident`, `category:availability-incident`, `category:feature`, or `category:improvement`, in that precedence order. Incident categories require an actual CIA impact, not mere importance. Never copy secrets or incident details into labels, Projects, or unnecessary Issue text. If classification is genuinely unclear, use the safe default `category:improvement` and state that queued work must be re-triaged by leaving exactly one category label.
5. If the matching Issue is `agent:running`, report its URL and state and stop. If it is `agent:queued`, ensure it has exactly the selected category and reuse it. Otherwise remove other `agent:*` state labels, ensure exactly the selected category, and apply `agent:queued`. If no match exists, create one Issue containing the objective, constraints, and completion criteria, then apply the category and `agent:queued` together.
6. Immediately after creating, re-queuing, or re-categorizing an Issue, run `bin/agentic-loop sync-issue ISSUE_NUMBER`. This best-effort command adds it to the repository Project before Supervisor claim and durably queues a retry when Projects is temporarily unavailable. Do not treat a deferred Project update as an Issue queue failure.
7. Re-read the Issue and verify it is open, has exactly one category, and is either queued or running. Report its URL, category, and state, then stop without implementing it in the interactive session.

If any queue prerequisite cannot be verified, identify the failed check and follow the safe fallback in `docs/operations/issue-queue.md`. Do not silently choose a route, create an unverified duplicate, or implement in parallel with queued work.

## Non-recursive worker exception

When already processing an `agent:running` Issue in its dedicated worktree, do not run queue intake or create a replacement Issue. Treat the original Issue and all comments as the requirements, then complete investigation, implementation, full validation, secret guard, commit, push, PR, required checks, review feedback, merge, default-branch verification, and cleanup.

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
