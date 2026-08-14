# CI Setup

Three jobs, all on GitHub's hosted runners:

1. **build-and-test** — build, vet, cross-compile to amd64, unit tests, and
   `make verify-reproducible`.
2. **asks** — ask/§ consistency (ADR-0009), via `scripts/check-asks.sh`.
3. **substrate-coverage** — asserts a recorded substrate run covers HEAD.

**None of them runs the substrate suite, and none can.** That is a property of hosted
runners, not an omission — see below.

## Why there is no substrate job

There was one. It targeted `[self-hosted, linux, btrfs, runsc]`, no runner was ever
registered, and it sat `queued` forever: it never passed and never failed. Per
`scripts/check-asks.sh`, *a gate that cannot fail is worse than no gate, because it is
believed.*

A hosted runner cannot run the suite, because it does not provide:

- Root access for `--network=sandbox` (needs `CAP_NET_ADMIN` + `CAP_SYS_ADMIN`)
- gVisor/runsc
- A btrfs filesystem for the quota enforcement tests
- Real netns / nftables / cgroup v2

So the suite runs on a substrate host, and CI checks that it *did*.

## How substrate coverage works

`scripts/check-substrate-coverage.sh` asserts two things, both mechanical:

1. A record in `docs/substrate-runs/` names a commit that is an **ancestor of HEAD**
   and whose run **exited 0**. A record from a sibling branch says nothing about this
   tree.
2. Every commit from that SHA to HEAD is one `test/preflight/substrategate` reports as
   **not needing the substrate** — the same tool and the same token-stream equivalence
   that licenses skipping the gate locally (ADR-0005).

Together: either the suite ran on this exact tree, or every change since provably
cannot affect a running sandbox.

The record is **tracked**, and it is **Markdown**. Both are forced. An untracked record
is one nothing can keep honest (the previous one was gitignored, and rotted into a false
claim while looking like evidence), and this check has to read it in a fresh checkout.
Markdown because `substrategate` classifies only `.md` as inert: a `.log` record would
make the commit that records a run itself require a run to certify it, which no commit
could ever satisfy.

A record carries **the commit SHA, the tag set, the exit code and the counts**. The SHA
is the load-bearing field — the old record had an exit code and a package list but no
SHA, so nothing could tell it predated M10 and M11 entirely.

## Recording a run

On a substrate host (dev/test: OrbStack machine `sbx`, per ADR-0002), from a clean tree:

```sh
scripts/record-substrate-run.sh            # all three tiers
scripts/record-substrate-run.sh --quick    # the routine substrate tier only
```

It refuses a dirty tree and re-checks HEAD afterwards: a record must describe a tree
that its SHA actually is. Commit the record it writes.

### Substrate host requirements

- **Linux** with root
- **gVisor/runsc** (systrap platform) on PATH
- **btrfs** for test scratch (`squota` mode), plus **btrfs-progs**
- **cgroup v2**, **netns**, **nftables** (`nft`), **iproute2** (`ip`)
- **Go 1.23+**

Test scratch goes under `/tmp` by default, so `/tmp` must be on btrfs — or point
`TMPDIR` at one. All of it must be on native fs, never a virtiofs shared mount
(ADR-0002).

## Tiers and cost

`-p 1` on every substrate invocation is not tuning: these packages launch real runsc
containers, and running them concurrently starves the in-guest health probes the
reaper's idle logic reads.

| Tier | Invocation | Cost |
| --- | --- | --- |
| Cheap gate | `go test ./...` | seconds, no root |
| Substrate | `-tags substrate -count=1 -p 1 ./...` | ~4–5 min on arm64 |
| drainrace | `-tags 'substrate,drainrace' -run TestPauseDrain ./test/` | ~60s |
| quotachurn | `-tags 'substrate,quotachurn' ./internal/btrfs` | seconds to ~30s, host-dependent |

`drainrace` and `quotachurn` are excluded from the routine gate because each costs
minutes; they are run deliberately. `README.md` carries both invocations.

For current counts, read the newest file in `docs/substrate-runs/` — it is generated
from a run rather than asserted, which is the point. A handful of skips is normal: read
the reasons, not the count. Anything that **fails** is a finding.

## What CI cannot tell you

A green cheap gate proves nothing about the concurrency and failure invariants this
engine's correctness lives in (§4.4 the three, §4.3 health classification, §2.4 egress
fail-closed). `substrate-coverage` is a freshness check on evidence produced elsewhere,
not evidence itself.

Branch protection is unavailable on a private repo on this plan, so **nothing here can
block a merge.** The pre-push hooks in `scripts/` are the actual enforcement; CI is the
second opinion. `scripts/install-hooks.sh` installs them.
