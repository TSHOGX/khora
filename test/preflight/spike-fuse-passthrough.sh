#!/usr/bin/env bash
# spike-fuse-passthrough.sh — M6 Spike 2: does a HOST-SIDE FUSE mountpoint
# survive gofer passthrough into a runsc guest under --file-access=shared? This
# de-risks the "local=dir / remote=FUSE" seam: a remote Volume backend mounts
# FUSE on the host (never inside the sandbox — gVisor's in-guest FUSE is the
# limited path), and the gofer proxies it in exactly like a plain dir (Spike 1).
#
# Proves the gofer descends INTO the FUSE mount (not treating the mountpoint as
# an empty dir on the underlying fs) and that reads/enumeration round-trip. The
# FUSE fs is read-only (minimal raw /dev/fuse server), so this validates the
# read/list path; the write path for a real backend is FUSE→backend, out of the
# gofer's concern.
#
# Guest probe uses dash builtins only (no coreutils in the bare rootfs — Spike 1
# lesson). Requires root. Expects the prebuilt fusehello binary path as $1.
set -euo pipefail

FUSEHELLO="${1:?usage: spike-fuse-passthrough.sh <path-to-fusehello-binary>}"
WORK="$(mktemp -d /tmp/spike-fuse.XXXXXX)"
ROOTFS="$WORK/rootfs"; FMNT="$WORK/fusemnt"; BUNDLE="$WORK/bundle"; RUNSC_ROOT="$WORK/runsc-root"
CID="spikefuse$$"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
SRV=""

cleanup() {
  sudo runsc --root="$RUNSC_ROOT" delete --force "$CID" 2>/dev/null || true
  sudo umount "$FMNT" 2>/dev/null || true
  [ -n "$SRV" ] && kill "$SRV" 2>/dev/null || true
  sudo rm -rf "$WORK"
}
trap cleanup EXIT

echo "== build minimal rootfs =="
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" /usr/bin/dash >/dev/null
mkdir -p "$ROOTFS/vol"

echo "== mount host-side FUSE fs at $FMNT =="
mkdir -p "$FMNT"
sudo "$FUSEHELLO" "$FMNT" 2>"$WORK/fuse.log" & SRV=$!
sleep 1
grep -q "$FMNT" /proc/mounts || { echo "FUSE mount failed"; cat "$WORK/fuse.log"; exit 1; }
echo "   mounted: $(grep "$FMNT" /proc/mounts)"
FMNT_REAL="$(readlink -f "$FMNT")"

# guest probe: enumerate + read the FUSE-backed file + nested dir (dash builtins)
GUEST='echo GLOB_START; for f in /vol/*; do echo GOT:$f; done; echo GLOB_END; if [ -f /vol/from_fuse ]; then read h < /vol/from_fuse; echo FUSE_CONTENT=$h; else echo FUSE_CONTENT=MISSING; fi; if [ -f /vol/subdir/nested ]; then read n < /vol/subdir/nested; echo NESTED_CONTENT=$n; else echo NESTED_CONTENT=MISSING; fi; echo DONE'

echo "== write OCI config.json: bind the FUSE mountpoint at /vol (ro) =="
mkdir -p "$BUNDLE"
cat > "$BUNDLE/config.json" <<JSON
{"ociVersion":"1.0.0",
 "process":{"terminal":false,"user":{"uid":0,"gid":0},
   "args":["/bin/sh","-c","$GUEST"],
   "env":["PATH=/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root"],"cwd":"/",
   "capabilities":{"bounding":["CAP_DAC_OVERRIDE"],"effective":["CAP_DAC_OVERRIDE"],"permitted":["CAP_DAC_OVERRIDE"]}},
 "root":{"path":"$ROOTFS","readonly":false},"hostname":"guest",
 "mounts":[{"destination":"/proc","type":"proc","source":"proc"},
   {"destination":"/dev","type":"tmpfs","source":"tmpfs","options":["nosuid","mode=755","size=65536k"]},
   {"destination":"/vol","type":"bind","source":"$FMNT_REAL","options":["rbind","ro"]}],
 "linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON

echo "== runsc run (pinned flags, --network=none) =="
set +e
OUT="$(sudo runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
  --overlay2=none --file-access=shared --systemd-cgroup=false \
  run --bundle="$BUNDLE" "$CID" 2>"$WORK/stderr")"
RC=$?
set -e
echo "--- guest stdout ---"; echo "$OUT"
echo "--- guest stderr ---"; cat "$WORK/stderr" || true
echo "--- runsc exit=$RC ---"

echo; echo "== VERDICT =="
FAIL=0
echo "$OUT" | grep -q "GOT:/vol/from_fuse"           && echo "PASS  gofer descended into FUSE mount (enumerated entries)" || { echo "FAIL  gofer did NOT see FUSE entries (treated mountpoint as empty?)"; FAIL=1; }
echo "$OUT" | grep -q "FUSE_CONTENT=FUSE_BACKED_CONTENT" && echo "PASS  guest read FUSE-backed file through gofer"       || { echo "FAIL  guest could not read FUSE-backed file"; FAIL=1; }
echo "$OUT" | grep -q "NESTED_CONTENT=FUSE_NESTED"    && echo "PASS  guest read FUSE-backed nested dir through gofer"   || { echo "FAIL  guest could not read FUSE nested dir"; FAIL=1; }
exit $FAIL
