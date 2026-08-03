#!/usr/bin/env bash
# spike-volume-mount.sh — §5.C Volume local arm: does runsc (--overlay2=none
# --file-access=shared) gofer-proxy an EXTRA rw host-dir bind mount into a live
# guest, alongside the (plain) rootfs? This is the local Volume backend's floor:
# a Volume = a host directory added to the OCI mount list.
#
# Strips everything already proven by the e2e (overlay, netns, transport): plain
# rootfs as root.path, --network=none, foreground `runsc run`. The guest probe
# uses ONLY dash builtins (glob + `read` + `echo >`) because the bare rootfs has
# no coreutils (no ls/cat) — reading via `cat` would spuriously look like a
# coherence failure when it is just a missing binary.
#
# Usage: sudo ./spike-volume-mount.sh    (needs root: runsc gofer + mount ns)
set -euo pipefail

WORK="$(mktemp -d /tmp/spike-vol.XXXXXX)"
ROOTFS="$WORK/rootfs"; VOL="$WORK/volume"; BUNDLE="$WORK/bundle"; RUNSC_ROOT="$WORK/runsc-root"
CID="spikevol$$"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"

cleanup() {
  sudo runsc --root="$RUNSC_ROOT" delete --force "$CID" 2>/dev/null || true
  sudo rm -rf "$WORK"
}
trap cleanup EXIT

echo "== build minimal rootfs (dash + loader, no overlay) =="
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" /usr/bin/dash >/dev/null

echo "== seed the host-side Volume dir (from_host + subdir/nested) =="
mkdir -p "$VOL/subdir"
echo "HOST_WROTE_THIS" > "$VOL/from_host"
echo "NESTED_OK"       > "$VOL/subdir/nested"
sync
mkdir -p "$ROOTFS/vol"
VOL_REAL="$(readlink -f "$VOL")"

# Guest probe, dash-builtins only:
#  - glob /vol/*        → enumerate what the gofer exposes at attach
#  - read < /vol/from_host   → read host-origin content (host→guest)
#  - echo > /vol/from_guest  → write back (guest→host, rw)
#  - read back own write      → self-consistency
GUEST='echo GLOB_START; for f in /vol/*; do echo GOT:$f; done; echo GLOB_END; if [ -f /vol/from_host ]; then read h < /vol/from_host; echo HOST_CONTENT=$h; else echo HOST_CONTENT=MISSING; fi; if [ -f /vol/subdir/nested ]; then read n < /vol/subdir/nested; echo NESTED_CONTENT=$n; else echo NESTED_CONTENT=MISSING; fi; echo GUEST_WROTE_THIS > /vol/from_guest; read g < /vol/from_guest; echo OWN_READBACK=$g; echo DONE'

echo "== write OCI config.json with an EXTRA rw bind mount at /vol =="
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
   {"destination":"/tmp","type":"tmpfs","source":"tmpfs","options":["nosuid","nodev","mode=1777"]},
   {"destination":"/vol","type":"bind","source":"$VOL_REAL","options":["rbind","rw"]}],
 "linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON

echo "== runsc run (foreground, pinned flags, --network=none) =="
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
echo "$OUT" | grep -q "HOST_CONTENT=HOST_WROTE_THIS"   && echo "PASS  host→guest file read"        || { echo "FAIL  host→guest file read"; FAIL=1; }
echo "$OUT" | grep -q "NESTED_CONTENT=NESTED_OK"       && echo "PASS  host→guest nested dir read"   || { echo "FAIL  host→guest nested dir read"; FAIL=1; }
echo "$OUT" | grep -q "GOT:/vol/from_host"             && echo "PASS  guest enumerates host entries" || { echo "FAIL  guest dir enumeration"; FAIL=1; }
echo "$OUT" | grep -q "OWN_READBACK=GUEST_WROTE_THIS"  && echo "PASS  guest self-write readback"     || { echo "FAIL  guest self-write readback"; FAIL=1; }
{ [ -f "$VOL/from_guest" ] && grep -q GUEST_WROTE_THIS "$VOL/from_guest"; } && echo "PASS  guest→host write reached host = $(cat "$VOL/from_guest")" || { echo "FAIL  guest→host write"; FAIL=1; }
exit $FAIL
