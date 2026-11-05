---
name: codex-pr-monitor
description: Register an owned GitHub pull request for the local Codex PR monitor, or remove it when ownership ends.
---

Register an open PR from its owning chat with `codex-pr-monitor add --url <PR URL> --thread <thread id>`. Start or stop the shared monitor with `codex-pr-monitor start` or `codex-pr-monitor stop`. Remove merged, closed, or no-longer-owned PRs with `codex-pr-monitor remove --url <PR URL>`.

The monitor routes possible submitted Bob reviews, known bot feedback, and current-head failed checks to the registered chat. Read the full feedback and apply your existing PR contract. Bob and agents share a login: use your checkpoint to exclude your own reviews and replies, and resolve uncertain authorship before acting. Delivery and filtering are best effort; a queue success does not prove the chat resumed. Keep existing monitoring until a coordinated handover is validated.
