#!/usr/bin/env bash
set -euo pipefail

# Watch source spike: the acceptance test records the measured contract rather
# than claiming syscall-complete history. It is intentionally kept as a small
# runner so a developer can repeat it on a runsc/overlay host.
repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"
go test -tags substrate -count=1 -run PlacementWatchDeliversRecursiveChange ./test/
