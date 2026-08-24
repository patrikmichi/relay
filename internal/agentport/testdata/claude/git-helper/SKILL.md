---
name: git-helper
description: Summarizes uncommitted git changes and flags risky diffs. Use when asked what changed or for a commit message.
allowed-tools:
  - Bash(git diff *)
  - Bash(git status *)
disable-model-invocation: false
license: MIT
---

## Instructions

Summarize the current git diff in two or three bullet points, then list any
risks you notice (missing error handling, hardcoded values, tests needing
updates).

Run the bundled helper script for a quick stat summary:

```bash
scripts/run.sh
```
