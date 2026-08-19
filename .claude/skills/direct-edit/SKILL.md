---
name: direct-edit
description: Make a sanctioned direct edit to a tracked file in this repository when the Claude Code edit-guard hook denies it. Use when a human explicitly asks to edit or fix files directly in an interactive session instead of through the Issue queue, when they need to change the primary (main) worktree itself, or when an Edit/Write/NotebookEdit is denied with a message naming `agentic-loop-allow-edit`. Explains how to open and close the per-worktree escape-hatch flag and clean up afterwards.
---

# Direct edit (edit-guard escape hatch)

This repository ships a Claude Code `PreToolUse` hook (`.claude/hooks/confirm-main-worktree-edit.sh`) that gates `Edit`/`Write`/`NotebookEdit` on **tracked** files. It exists so the normal path for change is queue-first — an Issue the Supervisor implements in a dedicated worker worktree — and so the autonomous loop can never edit the primary (main) worktree by accident. A `deny` from it is not a failure; it is the gate asking you to take the sanctioned path.

Untracked and scratch files are never gated. This skill is only about **tracked** files.

## First, prefer the queue

For an ordinary build or change request, do **not** open the escape hatch. Route it through the Issue queue with the `submit-requirement` skill: it becomes an Issue, the Supervisor claims it, and a worker implements it in its own worktree and opens a PR. Direct editing is the exception, reserved for cases where the queue is the wrong tool.

## When a direct edit is legitimate

- The user explicitly asks for synchronous / direct implementation in this interactive session ("直接直して", "このセッションで直接修正して", "queue を通さないで").
- The change must touch the **primary (main) worktree / root repository itself** — for example verifying behavior that a linked worktree cannot reproduce, or repairing the loop's own installed files locally.
- You are already working in a linked worktree (e.g. a PR worktree) and the user wants you to hand-edit it rather than file an Issue.

## How the escape hatch works

The hatch is a throwaway flag file named `agentic-loop-allow-edit` placed in the **target worktree's own gitdir**. It is scoped to exactly one worktree:

- Run from the **primary** worktree → opens direct edits to main.
- Run from a **linked** worktree → opens only that worktree.

Resolve the correct gitdir with `git rev-parse --absolute-git-dir` (it returns `<repo>/.git` in the primary worktree and `<repo>/.git/worktrees/<name>` in a linked one), so the same two commands work everywhere:

Open the hatch (run from inside the worktree you will edit):

```sh
touch "$(git rev-parse --absolute-git-dir)/agentic-loop-allow-edit"
```

Make the sanctioned edits, then **close it immediately**:

```sh
rm -f "$(git rev-parse --absolute-git-dir)/agentic-loop-allow-edit"
```

The flag lives under `.git/`, so it is never committed and never distributed; it is purely local and temporary.

## Your responsibility as the agent

When the user has explicitly authorized direct editing for this session, **you open the hatch yourself before editing and remove it as soon as the edits are done** — do not leave it open, and do not ask the user to run the commands. Treat the open hatch as a held lock: the shortest possible window around exactly the intended edits.

If instead you were denied while doing something that should go through the queue, do not open the hatch — file the Issue (`submit-requirement`) and stop.

## What the deny messages mean

- **main(primary) worktree, human** — "原則禁止": the default is to work in a worktree via the queue; open the primary hatch only for a genuine root-repository exception.
- **linked worktree, human** — file an Issue for the Supervisor, or open that worktree's hatch for a deliberate manual change.
- **any worktree, autonomous loop** — the autonomous loop (marked by `AGENTIC_LOOP_AGENT`) may edit its own linked worktree freely but is always blocked from the primary worktree. Autonomous agents never use this hatch; if you see this deny, an autonomous run is trying to touch main and should be corrected, not unblocked.
