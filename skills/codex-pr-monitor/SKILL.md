---
name: codex-pr-monitor
description: Register an owned GitHub pull request for the local Codex PR monitor, or remove it when ownership ends.
---

Register an open PR from its owning chat with `codex-pr-monitor add --url <PR URL> --thread <thread id>`. Start or stop the shared monitor with `codex-pr-monitor start` or `codex-pr-monitor stop`. Merged or closed PRs are removed automatically after notification; register them again if reopened. Remove no-longer-owned PRs with `codex-pr-monitor remove --url <PR URL>`.

When notified, run the `inbox` command in the message. Handle every listed item under your existing PR contract, including recording any blocker, then run the exact `ack` command returned by the inbox. Never acknowledge unhandled items. New arrivals remain pending for a later wake. Read the full feedback; Bob and agents share a login, so use your checkpoint to identify your own output. Delivery is best effort.
