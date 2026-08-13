---
name: submit-requirement
description: Convert a natural-language product goal, change request, bug report, or improvement to the agentic development loop itself into autonomous repository work. Use when the user states what they want built or changed, even when constraints or acceptance criteria are incomplete.
---

# Submit Requirement

Turn the user's short request into a verified, merged change without making them manage the engineering workflow.

## Queue-first intake

For an ordinary build or change request received in an interactive session, use the repository Issue queue instead of starting implementation when the queue is configured and GitHub Issue read/write access and the resulting Issue state can be verified. Supervisor health controls when work is claimed, not whether a request can be persisted:

1. Exclude read-only questions, diagnosis, status checks, and operational commands such as `start` and `stop`. Also skip intake when the user explicitly requests synchronous or direct implementation.
2. Verify `.agentic-loop.toml`, executable `bin/agentic-loop`, GitHub repository access, and GitHub Issue read/write access. Run `bin/agentic-loop status` to observe whether the Supervisor is running, but do not require it to be running for intake.
3. Choose the duplicate-check path before making API calls:
   - If the user explicitly asks to create a new Issue (for example, "create an Issue" or "Issueを作って"), do not search for duplicates and create a new Issue. If the same request explicitly says to reuse an existing Issue or check for duplicates, search instead.
   - For an ordinary natural-language build or change request that is being routed automatically, inspect only active Issues (open Issues carrying an `agent:*` state label). Search their titles and bodies, then inspect comments only for plausible matches. Reuse an Issue that asks for the same user-visible outcome.
   - For code diagnosis and scheduled audits, preserve their stricter rules: search both open and closed Issues and do not create a duplicate finding.
4. Classify the requirement with exactly one category label: `category:loop-continuity`, `category:confidentiality-incident`, `category:integrity-incident`, `category:availability-incident`, `category:feature`, or `category:improvement`, in that precedence order. Incident categories require an actual CIA impact, not mere importance. Never copy secrets or incident details into labels, Projects, or unnecessary Issue text. If classification is genuinely unclear, use the safe default `category:improvement` and state that queued work must be re-triaged by leaving exactly one category label.
5. If the matching Issue is `agent:running`, report its URL and state and stop. If it is `agent:queued`, ensure it has exactly the selected category and reuse it. Otherwise remove other `agent:*` state labels, ensure exactly the selected category, and apply `agent:queued`. If no match exists, create one Issue containing the objective, constraints, and completion criteria, then apply the category and `agent:queued` together. If the paths or external environments the change will touch are known, add a single-line `<!-- agentic-loop:scope paths=a,b env=c -->` marker to the Issue body (see "変更競合の予防" in `docs/operations/issue-queue.md`) so the Supervisor can avoid claiming it alongside another Issue with an overlapping change scope. Omit the marker when unknown; the queue falls back to a safe default (`unknown_scope`) rather than serializing the whole repository. If the requirement depends on another Issue finishing first, register it with GitHub's native issue dependencies (`Blocked by`) or, when that is unavailable, add a single body line `Blocked by: #12, #34` (one line, `#`-prefixed numbers in this same repository only; no other repository references or extra lines). The Supervisor will not claim the Issue until every declared dependency is closed and verified complete.
6. Immediately after creating, re-queuing, or re-categorizing an Issue, run `bin/agentic-loop sync-issue ISSUE_NUMBER`. This best-effort command adds it to the repository Project before Supervisor claim and durably queues a retry when Projects is temporarily unavailable. Do not treat a deferred Project update as an Issue queue failure.
7. Re-read the Issue and verify it is open, has exactly one category, and is either queued or running. Report its URL, category, and state, then stop without implementing it in the interactive session. If the Supervisor status observed in step 2 was stopped, also report that the Issue is registered but will not be claimed until the Supervisor is started.

Supervisor stopped status is not a queue prerequisite failure. A Project sync failure is also not an intake failure when `sync-issue` has durably queued its retry. If queue files, GitHub repository access, GitHub Issue read/write access, or the final open/category/state invariants cannot be verified, identify the failed check and follow the safe fallback in `docs/operations/issue-queue.md`. Do not claim success, silently choose a route, create another Issue to probe the failure, or implement in parallel with possibly queued work.

## Non-recursive worker exception

When already processing an `agent:running` Issue in its dedicated worktree, do not run queue intake or create a replacement Issue. Treat the original Issue and all comments as the requirements, then complete investigation, implementation, full validation, secret guard, commit, push, PR, required checks, review feedback, merge, default-branch verification, and cleanup.

1. Inspect the repository, its `AGENTS.md`, current behavior, and relevant external facts.
2. Infer safe, reversible details. Ask only when a missing decision has material cost, security, availability, data-loss, or product consequences.
3. State the interpreted objective, constraints, invariants, and observable completion criteria briefly; then proceed without waiting for confirmation when the interpretation is safe.
4. Sync the default branch and create a dedicated branch in a separate Git worktree. Never develop directly on the default branch.
5. Investigate, design, implement, test, and repair until every completion criterion and invariant is demonstrably satisfied.
6. Run the repository's full validation and secret guard. Do not bypass hooks or expose credentials in commands, logs, commits, or PR text.
7. Commit, push, open a PR, monitor required checks, address failures and review feedback, and merge the PR. Remove the worktree and branch after a successful merge.
8. Report the outcome, verification evidence, PR link, and any genuinely unresolved risk concisely. A worker must not cancel, supersede, merge, or auto-requeue a disposed Issue. If it judges work unnecessary, leave the rationale and return `AGENTIC_LOOP_RESULT=needs-input`; only an authorized operator may use `bin/agentic-loop dispose` or `resume`.

Required checks、AI review、merge待ちは、同一turnで前景実行する。`gh pr checks --watch` などを時間上限付きで実行し、pendingなら状態を再確認して前景で繰り返す。background process、別agent、別sessionに待機を委譲しない。checksが未確定、review feedbackが未対応、mergeまたはdefault branchでの検証が未完了のまま「待機中です」等で終了してはならない。失敗時は修正して必要な検証を再実行し、mergeとdefault branch検証まで完遂するか、正当な終端状態へ分類するまで継続する。

workerの最終応答は、最後の非空行を `AGENTIC_LOOP_RESULT=completed`、`AGENTIC_LOOP_RESULT=failed`、`AGENTIC_LOOP_RESULT=needs-input`、`AGENTIC_LOOP_RESULT=declined` のいずれか一行だけにする。markerの後に説明やコードフェンスを続けず、自由文の待機報告を完了結果として扱わない。

Write GitHub Issue and PR titles, bodies, comments, and reviews in Japanese. Preserve code, logs, identifiers, proper nouns, and quotations in their necessary original form.

Treat requests about this workflow exactly like application requests. Improve the loop when doing so is required by the user's goal.
