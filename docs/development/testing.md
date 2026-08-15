# Testing and verification

## Deterministic checks

A ready-for-review change reports these results:

```sh
go build ./...
go vet ./...
GOARCH=amd64 go build ./...
go test ./...
./scripts/check-generated.sh
```

CI also runs the retired-reference negative fixtures and reproducible-build check.
The workflow is [ci.yml](../../.github/workflows/ci.yml); it installs pinned generator
versions before checking derived output.

Unit and contract coverage scales with the boundary being changed. Store and Volume
backends share contract suites. Lifecycle seams include concurrent transitions and
fault injection. Architectural tests inspect imports under production and tagged
builds. A structural test must demonstrate both that current code passes and an
injected prohibited shape fails.

## Decide on substrate acceptance

Evaluate the complete PR diff plus working tree after the last material change:

```sh
go run ./test/preflight/substrategate --base origin/main
```

Exit `0` means the real-substrate suite is not required. Exit `1` means it is
required. Exit `2` means the tool could not decide and must also be treated as
required. Documentation, repository process files, unit-test-only changes, and
lexically unchanged Go edits may be classified as inert; runtime inputs and substrate
tests fail closed.

## Run the real substrate suite

When required, use a Linux host with root, runsc, overlayfs, netns, cgroup v2,
btrfs `squota`, `nft`, `ip`, and native filesystem storage. The default suite is:

```sh
sudo -E go test -tags=substrate ./...
```

Long-running cases have additional tags, including `drainrace` and `quotachurn`;
run them when the corresponding mechanism changes. The centralized preconditions
are in [test/substratereq](../../test/substratereq), and individual tests skip only
through that profile.

Record `passed`, `failed`, or `not required` in the PR with the exact command, host
profile, result, and every skip reason. Re-run the decision after a later material
change. Do not commit a substrate transcript, and do not present hosted CI as a
local substrate result.

When a substrate test fails, reproduce it through the harness and inspect its
captured namespace, rule, cgroup, and filesystem evidence before assigning the
failure to the engine.
