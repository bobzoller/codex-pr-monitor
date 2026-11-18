# codex-pr-monitor

A small local, shared PR monitor that routes possible material changes to the owning Codex chat. It does not modify PRs. Reviewers must assess feedback and follow their existing authorization and checkpoint rules.

## Setup

Requires `gh` on PATH (authenticated with `gh auth login` or `GITHUB_TOKEN`) and Codex CLI on PATH. On macOS, the monitor also looks for the CLI inside ChatGPT.app. If both `GITHUB_TOKEN` and `GH_TOKEN` are set, the monitor uses `GITHUB_TOKEN`. The process inherits its startup environment.

For a published release, download the archive for your system (`darwin_arm64`, `darwin_amd64`, `linux_arm64`, or `linux_amd64`) and verify its checksum. For example:

```sh
VERSION=v1.0.0  # replace with the release to install
PLATFORM=darwin_arm64  # replace with your OS and architecture
ARCHIVE="codex-pr-monitor_${VERSION}_${PLATFORM}.tar.gz"
gh release download "$VERSION" --repo bobzoller/codex-pr-monitor --pattern "$ARCHIVE" --pattern SHA256SUMS.txt
grep -F "  $ARCHIVE" SHA256SUMS.txt | shasum -a 256 -c -
tar -xzf "$ARCHIVE"
mkdir -p ~/.bin ~/.codex/skills/codex-pr-monitor
install -m 755 codex-pr-monitor ~/.bin/codex-pr-monitor
install -m 644 skills/codex-pr-monitor/SKILL.md ~/.codex/skills/codex-pr-monitor/SKILL.md
```

Or clone the repository and build with Go 1.21.13 or newer using `CGO_ENABLED=0 go install .`. Copy `skills/codex-pr-monitor` into your Codex skills directory. Put the binary directory on PATH, then register and start:

```sh
codex-pr-monitor add --url https://github.com/OWNER/REPO/pull/123 --thread THREAD_ID
codex-pr-monitor start
codex-pr-monitor stop
codex-pr-monitor remove --url https://github.com/OWNER/REPO/pull/123
codex-pr-monitor inbox --url https://github.com/OWNER/REPO/pull/123 --thread THREAD_ID
```

Each PR has one owning thread; adding the same URL updates its owner. Removal retains event history and discards pending inbox items and wakes, so re-adding does not replay previously seen events.

The daemon batches PR state and review feedback for up to five PRs per GitHub GraphQL request, with paginated REST fallbacks when needed. It checks CI through REST to preserve existing event identities. Rejected batches are split or retried, partial failures leave affected PRs for a later check, and GitHub rate-limit responses pause requests until the indicated retry or reset time (with backoff when no time is supplied). It polls registered PRs at startup and every minute thereafter, at any hour. With no registrations it makes no GitHub requests. The first poll after downtime catches events still visible in GitHub’s current results; transient failures that resolve while offline can be missed. The machine must be awake and the daemon running; it does not launch automatically at login. `start` is reentrant and `stop` is repeatable. State, lock, PID, and `monitor.log` are under `~/.config/codex-pr-monitor`; the log is truncated on each new daemon start. The database includes PR URLs, thread IDs, and event identities; credentials are never written to the log.

The monitor notifies the owning chat when a registered PR is merged or closed, including if it was already closed when registered. After a wake has been queued, it removes the registration automatically and retains the terminal item in the inbox until acknowledged. If the PR is reopened, register it again to resume monitoring; a later closure can trigger a new notification. Queue failures retain the registration for retry. Previously notified terminal registrations left by an older version are removed without sending another notification.

For open PRs, the monitor checks submitted reviews by the authenticated GitHub user (including inline reviews), feedback from a small hard-coded set of known bot accounts, and failures on the current head. It deliberately ignores other humans and standalone comments by that user: agents may share the login. Bot feedback is a candidate, not a determination of actionability, and may include benign summaries. Agents must verify authorship against their own checkpoints and judge feedback themselves. No generic API identifies an agent-authored review with certainty, and this monitor cannot read each chat's checkpoint. A submitted review edited after its original submission may not be re-notified. Bot coverage is limited to the accounts in `main.go`; new bots require a code change. If the authenticated user cannot be resolved, user reviews remain eligible for a later poll while bot, CI, and PR-state checks continue. GitHub access failures preserve state and retry at the next poll.

The monitor saves event identities and details in SQLite before sending a short wake-up. While that wake is outstanding, new events accumulate in the same per-PR, per-thread inbox instead of creating more messages. Run the `inbox` command from the wake, handle all listed items, and run the exact `ack` command it prints. That command acknowledges only the snapshot read; items arriving later remain pending and receive a subsequent wake. An interrupted or blocked agent should acknowledge only items it handled or for which it recorded a blocker. `inbox` and `ack` accept `--thread`; when omitted, they use `CODEX_THREAD_ID` or the sole pending thread. A manual `remove` discards pending inbox items; a message already queued to the chat cannot be recalled.

Queue failures leave the inbox intact for the next poll. Codex can report success without confirming that the chat resumed; a wake still unacknowledged after two hours is retried once, then logged if it remains unacknowledged. This bounded recovery can create a duplicate if an agent is still working. A crash while queueing can also produce a duplicate. The monitor remains best effort while the machine, process, authentication, and GitHub are available; inspect `monitor.log` and the owning chat if a wake is not handled.

Run `CGO_ENABLED=0 go test ./...` for mock-based tests; they do not contact GitHub or real threads.

## Releases

Pushes to `main` test and build all targets. Pushing a version tag such as `v1.0.0` also publishes four OS/architecture archives and `SHA256SUMS.txt`. Tags containing a prerelease suffix publish a prerelease. To build the archives locally, run `scripts/build-release.sh v1.0.0 dist`.
