# Getting started

This walkthrough builds the binaries, creates the built-in `bare` Template,
starts the orchestrator, creates a sandbox, runs one process, and pauses it.

## Check the host

Use a Linux host with root access, `runsc`, overlayfs, network namespaces, cgroup
v2, a btrfs filesystem mounted with `squota`, `btrfs-progs`, `nft`, and `ip`.
Keep khora state and overlay scratch data on a native filesystem. The complete
requirements and inspection commands are in [Host requirements](configuration/host.md).

## Build the binaries and Template

```sh
make all
sudo mkdir -p /var/lib/khora/templates
sudo ./templates/bare/build.sh \
  /var/lib/khora/templates/bare \
  ./bin/sandboxd
```

The Template directory name is its ID, so this artifact is named `bare`. The
build procedure is defined by [templates/bare/build.sh](../templates/bare/build.sh).

## Start the orchestrator

```sh
sudo ./bin/orchestrator \
  --state /var/lib/khora/state \
  --templates /var/lib/khora/templates
```

The default listener is `127.0.0.1:8080`. In another terminal, verify readiness:

```sh
curl --fail http://127.0.0.1:8080/readyz
./bin/khora template list
```

## Create and use a sandbox

Create with explicit caps. Explicit caps are required for every node capacity
axis that the orchestrator meters.

```sh
./bin/khora sandbox create \
  --template bare \
  --cpu-millis 1000 \
  --memory-byte 1073741824 \
  --disk-byte 2147483648
```

The command prints the opaque sandbox ID. Use it below as `sbx_...`:

```sh
./bin/khora process start --sandbox sbx_... /bin/sh --arg=-c \
  --arg='printf "hello from khora\n"'
./bin/khora files upload sbx_... ./README.md /tmp/README.md
./bin/khora files download sbx_... /tmp/README.md -
./bin/khora sandbox pause sbx_...
./bin/khora sandbox resume sbx_...
```

Pause removes the live Placement but preserves the Sandbox record and its
Overlay. Resume cold-starts a new Placement from the same Template and Overlay;
it does not restore process memory. See [Lifecycle](concepts/lifecycle.md).

## Clean up

Pause before deleting a sandbox:

```sh
./bin/khora sandbox pause sbx_...
./bin/khora sandbox delete sbx_...
```

For a non-loopback deployment, do not expose the orchestrator directly. Follow
[Deployment boundary](configuration/deployment-boundary.md).
