# CI Setup

The CI workflow has two jobs:

1. **build-and-test** — runs on GitHub's hosted runners. No special requirements.
2. **substrate** — needs a self-hosted runner with real kernel features.

## Self-hosted runner requirements

The substrate job needs:

- **Linux host** with root access
- **gVisor/runsc** (systrap platform) installed and on PATH
- **btrfs filesystem** mounted and available for test scratch
- **cgroup v2**, **netns**, **nftables** (`nft`), **iproute2** (`ip`)
- **btrfs-progs** (the `btrfs` command)
- **Go 1.23+**

### Runner labels

The substrate job requires these labels on the runner:

- `self-hosted`
- `linux`
- `btrfs`
- `runsc`

Apply them when registering the runner, or add them afterward in the repository settings.

### Environment setup

The test suite creates its scratch directories under `/tmp` by default. Ensure `/tmp` is
on a btrfs filesystem, or point `TMPDIR` to one before starting the runner service.

The runner service must have passwordless sudo configured for the test commands, since
the substrate suite runs as:

```sh
sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 -p 1 ./...
```

### Why self-hosted

GitHub's hosted runners don't provide:

- Root access for `--network=sandbox` (which requires `CAP_NET_ADMIN` + `CAP_SYS_ADMIN`)
- gVisor/runsc installed
- A btrfs filesystem for quota enforcement tests
- Real netns / nftables / cgroup v2 for the networking and resource-cap acceptance tests

The cheap gate (build + vet + unit tests) runs on hosted runners and catches most
regressions. The substrate suite runs only when the cheap gate passes, on a runner you
control.

### Expected test counts

- **Cheap gate** (`go test ./...`): completes in seconds
- **Substrate suite** (`-tags substrate`): 908 passing tests, ~200–220s on arm64
- **drainrace** (`-tags substrate,drainrace`): ~60s, one legitimate skip is normal
- **quotachurn** (`-tags substrate,quotachurn`): ~3.5–26.5s depending on host load

A handful of skips is normal (documented in the goal file). Anything that **fails** is a
finding.
