---
name: diagnose-codebase
description: Audit a repository for requirements-to-implementation drift, undocumented behavior, structural disorder, missing development skills, and obsolete files, then record actionable findings as GitHub Issues without changing the code. Use for scheduled or manual codebase health checks and repository cleanup audits.
---

# Diagnose Codebase

Audit the repository without modifying tracked or untracked files.

1. Read `AGENTS.md`, requirements, policies, decisions, operations documentation, source, tests, CI configuration, and the directory tree. Treat repository-specific invariants as mandatory.
2. Compare documented requirements with observable implementation and tests in both directions. Use `bin/agentic-loop trace --audit --format json` as one evidence source: it flags Issues whose merged PR record left a requirement `unmet`/`partial` (implementation or verification missing) or left changed paths `unreferenced_paths` (implementation without a stated requirement), from existing verdict comments at no extra cost beyond a repository-wide comment read. Use `bin/agentic-loop capabilities --format json` as another evidence source when `.agentic-loop/capabilities.toml` exists: a `warning`/`failure` finding there (an unsafe or unreadable declaration, a stale `undetermined` item that is now detectable, or drift against `.agentic-loop.toml`'s `[foundation].verify_command`) is itself a diagnosable finding. Use `bin/agentic-loop flaky --format json` as another evidence source when `tests/flaky-registry.toml` exists: an expired or otherwise invalid quarantine entry, or an entry whose linked repair Issue never got fixed, is itself a diagnosable finding.
3. Assess whether directories have clear ownership, naming, and boundaries.
4. Identify repeated development work that a new or updated skill could make safer or faster.
5. Find files that are generated, superseded, unreachable, duplicated, or otherwise apparently unused. Never delete them during diagnosis.
6. Verify every finding with concrete paths and evidence. Do not file speculative findings that lack an observable impact or verification method.
7. Search open and closed Issues before filing. If an equivalent open Issue exists, do not duplicate it. A closed Issue may be reopened only when the same defect demonstrably recurred.
8. Create one Japanese GitHub Issue per independently actionable finding with the `diagnosis`, `category:improvement`, and `agent:queued` labels. Include the problem, evidence, expected state, suggested acceptance criteria, and affected paths. Immediately run `bin/agentic-loop sync-issue ISSUE_NUMBER` for each created Issue so Project visibility does not wait for Supervisor claim; a temporary Projects failure is safely queued for retry and must not stop diagnosis. Adding `agent:queued` delegates the fix to the existing Supervisor and Issue worker workflow; do not modify the code during diagnosis.
9. If no actionable finding exists, create no Issue and report that result.

Keep Issue titles, bodies, and comments in Japanese. Never commit, edit, delete, push, open a pull request, or change Issue state/labels other than assigning `diagnosis`, `category:improvement`, and `agent:queued` while creating a diagnosis Issue.
