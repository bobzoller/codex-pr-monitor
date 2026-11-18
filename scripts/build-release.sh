#!/usr/bin/env bash
set -euo pipefail

version=${1:?usage: build-release.sh VERSION OUTPUT_DIR}
output=${2:?usage: build-release.sh VERSION OUTPUT_DIR}
if [[ ! $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
  echo "expected a version tag such as v1.2.3 or v1.2.3-rc.1" >&2
  exit 1
fi
mkdir -p "$output"
output=$(cd "$output" && pwd)
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
mkdir -p "$staging/skills/codex-pr-monitor"
cp README.md "$staging/README.md"
cp skills/codex-pr-monitor/SKILL.md "$staging/skills/codex-pr-monitor/SKILL.md"
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "$staging/codex-pr-monitor" .
  archive="codex-pr-monitor_${version}_${os}_${arch}.tar.gz"
  tar -C "$staging" -czf "$output/$archive" codex-pr-monitor README.md skills/codex-pr-monitor/SKILL.md
done
(cd "$output" && shasum -a 256 "codex-pr-monitor_${version}_darwin_amd64.tar.gz" "codex-pr-monitor_${version}_darwin_arm64.tar.gz" "codex-pr-monitor_${version}_linux_amd64.tar.gz" "codex-pr-monitor_${version}_linux_arm64.tar.gz" > SHA256SUMS.txt)
