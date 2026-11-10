# codex-pr-monitor

A small local, shared PR monitor that routes possible material changes to the owning Codex chat. It does not modify PRs. Reviewers must assess feedback and follow their existing authorization and checkpoint rules.

## Setup

Requires Go 1.21+, `gh` on PATH (authenticated with `gh auth login` or `GITHUB_TOKEN`), and Codex CLI on PATH or at `/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`. If both `GITHUB_TOKEN` and `GH_TOKEN` are set, the monitor uses `GITHUB_TOKEN`. The process inherits its startup environment. Install with:

```sh
CGO_ENABLED=0 go install .
codex-pr-monitor add --url https://github.com/OWNER/REPO/pull/123 --thread THREAD_ID
codex-pr-monitor start
codex-pr-monitor stop
codex-pr-monitor remove --url https://github.com/OWNER/REPO/pull/123
```

Put Go's bin directory on PATH. Copy `skills/codex-pr-monitor` into your Codex skills directory if you want agents to discover it. Each PR has one owning thread; adding the same URL updates its owner. Removal retains the event history, so re-adding does not replay previously queued events.

The daemon batches PR state and review feedback for up to five PRs per GitHub GraphQL request, with paginated REST fallbacks when needed. It checks CI through REST to preserve existing event identities. Rejected batches are split or retried, partial failures leave affected PRs for a later check, and GitHub rate-limit responses pause requests until the indicated retry or reset time (with backoff when no time is supplied). It polls registered PRs at startup during local working hours and every minute thereafter, weekdays 08:00–18:00 in the machine's local time. The first poll after downtime or overnight catches events still visible in GitHub’s current results; transient failures that resolve while offline can be missed. It does not launch automatically at login. `start` is reentrant and `stop` is repeatable. State, lock, PID, and `monitor.log` are under `~/.config/codex-pr-monitor`; the log is truncated on each new daemon start. The database includes PR URLs, thread IDs, and event identities; credentials are never written to the log.

The monitor notifies the owning chat when a registered PR is merged or closed, including if it was already closed when registered. After successfully queueing that notification, it removes the registration automatically. If the PR is reopened, register it again to resume monitoring; a later closure can trigger a new notification. Queue failures retain the registration for retry. Previously notified terminal registrations left by an older version are removed without sending another notification.

For open PRs, the monitor checks submitted `bobzoller` reviews (including inline reviews), feedback from a small hard-coded set of known bot accounts, and failures on the current head. It deliberately ignores other humans and Bob's standalone comments: agents and Bob share a login. Bot feedback is a candidate, not a determination of actionability, and may include benign summaries. Agents must verify authorship against their own checkpoints and judge feedback themselves. No generic API identifies an agent-authored review with certainty, and this monitor cannot read each chat's checkpoint. A submitted review edited after its original submission may not be re-notified. Bot coverage is limited to the accounts in `main.go`; new bots require a code change. GitHub access failures preserve state and retry at the next poll.

Events are remembered after the queue command reports success. Codex queues the message before attempting to resume: when it warns that resume failed, the monitor logs the warning and does not enqueue a duplicate. The message may be consumed by an already-running chat or remain queued until the chat resumes; check the chat if needed. Queue success is **not confirmed consumption**. A failed queue command is retried on the next poll. A crash between queueing and recording can cause a duplicate. Removed PRs may still have an in-flight notification. This is best-effort monitoring while the machine, process, authentication, and GitHub are available. Check the log and retain existing monitors until an end-to-end handover is validated; this project does not start a service or change existing automations.

Run `CGO_ENABLED=0 go test ./...` for mock-based tests; they do not contact GitHub or real threads.
