---
name: git-helper
description: Summarizes uncommitted git changes and flags risky diffs. Use when asked what changed or for a commit message.
metadata:
  author: acme
  team: platform
---

## Instructions

Summarize the current git diff in two or three bullet points, then list any
risks you notice (missing error handling, hardcoded values, tests needing
updates).

Run the bundled helper script for a quick stat summary:

```bash
scripts/run.sh
```
