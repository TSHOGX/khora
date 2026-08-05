#!/usr/bin/env bash
# spike-mount-dest-upper.sh — §5.B Promote × §5.C Volume: when a Volume is bound
# to a guest path that does NOT exist in the merged rootfs, WHO creates the
# destination directory, and does that creation land in the Overlay UPPER?
#
# Why this matters (rootfs-overlay D5, "excluded paths / portal paths"): the
# excluded-paths list must eventually carry an entry for "Volume mount-point
# placeholder", and D5 says its shape cannot be settled until Volume lands.
# §5.C's 挂载拓扑规则 asserts the mount-point HOST DIRECTORY belongs to the
# Overlay artifact (the sandbox rootfs). If runsc/the kernel materializes that
# directory into the upper, then PromoteToTemplate — a byte-for-byte copy of the
# upper — freezes a PHANTOM empty dir (or worse, a whiteout / xattr-carrying
# node) into every Template promoted from a sandbox that ever had a Volume.
# This spike measures exactly what lands there.
#
# The chain reproduced here is the real one (rootfs-overlay D0/D3, matching
# internal/rootfs/overlayfs.go Mount): host kernel overlayfs
# lower(Template)+upper+work → merged, merged handed to
# `runsc --overlay2=none --file-access=shared` as root.path, and the Volume
# added as an extra OCI bind mount whose destination is inside merged.
#
# Probes (each states WHY it exists):
#   P1  dest /data absent from BOTH lower and upper → does launch fail, or does
#       something create it? Plus, in the same launch, the REVERSE hazard: a
#       guest write to /data must reach the Volume (host source dir), never the
#       upper — a leak there would be captured by Promote AND charged to the
#       sandbox's §3.4 disk cap instead of the Volume's (§5.C per-volume 帽).
#   P2  relaunch on the SAME upper WITHOUT the bind → is the artifact P1 left a
#       durable placeholder the guest still sees? This is what a Promoted
#       Template would look like to its children.
#   P3  dest EXISTS in the lower (Template) but not the upper → does binding
#       onto it force a copy-up into the upper? If yes, EVERY Volume mount point
#       dirties the upper regardless of Template contents.
#   P4  NESTED absent dest /srv/data/vol1 (no component exists) → is the whole
#       chain created, and does the whole chain land in the upper? Determines
#       whether the excluded-paths entry needs prefix/subtree semantics rather
#       than an exact path.
#   P5  ATTRIBUTION: `runsc create` and inspect the upper BEFORE `start`. P1–P4
#       cannot separate "runsc materialized the mount point" from "the guest
#       touched the path", and the difference matters: if it happens at create,
#       the placeholder exists in every sandbox that was ever placed with a
#       Volume — even one whose guest never ran — so Promote cannot treat it as
#       guest debris. P5 also uses a source dir with a DISTINCTIVE mode (0711)
#       to see whether the placeholder inherits the Volume's mode or gets a
#       fixed one, since whatever it is gets frozen into the layer.
#   P6  SHADOWED CONTENT: the dest already exists in the upper WITH files in it
#       (a guest wrote /data/... during an earlier placement that had no Volume;
#       §5.C says the mount table changes at the resume boundary, so this is
#       reachable). The bind then shadows those bytes. They are invisible to the
#       guest, still charged to the sandbox's §3.4 cap, and still copied by
#       Promote — i.e. a frozen layer that does not match what any guest ever
#       saw. Decides whether the exclusion must be a SUBTREE (dropping the
#       shadowed bytes) or merely the placeholder dir.
#
# Guest probes use ONLY dash builtins (glob, `read <`, `echo >`, `[`): the bare
# rootfs from build-rootfs.sh has no coreutils, so `ls`/`cat` would fail for
# reasons unrelated to what we are measuring. Two further constraints on guest
# code: no `2>/dev/null` (/dev is a bare tmpfs with no null device, so the
# redirect would itself be the failure), and no DOUBLE QUOTES — the script is
# embedded in the OCI config.json `args`, where a `"` closes the JSON string
# (measured: runsc rejects the bundle with `invalid character '$' after array
# element`). Bare `$f` is safe here; the probe paths contain no spaces.
#
# Isolation (other agents run runsc concurrently): --network=none, a private
# --root, and a PID-derived CID prefix. No netns is created.
#
# Usage: sudo ./spike-mount-dest-upper.sh   (needs root: mount(2) + runsc gofer)
set -euo pipefail

# Scratch MUST be on a real native fs that supports being an overlayfs upper —
# /var/tmp is btrfs on `sbx`, /tmp is tmpfs (ADR-0002: never the virtiofs share).
WORK="$(mktemp -d /var/tmp/spike-s3dest.XXXXXX)"
LOWER="$WORK/lower"        # the Template (read-only lower)
UPPER="$WORK/upper"        # the Overlay artifact — the thing Promote copies
WORKDIR="$WORK/work"       # overlayfs workdir (same fs as upper, D6.3)
MERGED="$WORK/merged"      # what runsc gets as root.path
VOL="$WORK/volume"         # the host-side Volume source dir
BUNDLE="$WORK/bundle"
RUNSC_ROOT="$WORK/runsc-root"
CID="s3dest$$"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
FAIL=0

cleanup() {
  for p in p1 p2 p3 p4 p5 p6; do
    sudo runsc --root="$RUNSC_ROOT" delete --force "${CID}${p}" 2>/dev/null || true
  done
  sudo umount "$MERGED" 2>/dev/null || sudo umount -l "$MERGED" 2>/dev/null || true
  sudo rm -rf "$WORK"
}
trap cleanup EXIT

# ---------------------------------------------------------------- helpers ----

# mount_overlay: assemble the merged view with the engine's exact option shape
# (internal/rootfs/overlayfs.go: lowerdir=<colon-joined>,upperdir=,workdir=).
# A FRESH empty workdir per mount is a kernel requirement (D5).
mount_overlay() {
  sudo rm -rf "$WORKDIR"
  sudo mkdir -p "$UPPER" "$WORKDIR" "$MERGED"
  sudo mount -t overlay overlay \
    -o "lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR" "$MERGED"
}

umount_overlay() {
  sudo umount "$MERGED" 2>/dev/null || sudo umount -l "$MERGED" 2>/dev/null || true
}

# reset_overlay: umount, WIPE the upper, remount. Used between probes so each
# observation of the upper is attributable to exactly one probe.
reset_overlay() {
  umount_overlay
  sudo rm -rf "$UPPER" "$WORKDIR"
  mount_overlay
}

# dump_upper <probe-tag> <label>: the core measurement. `ls -laR` shows entry
# TYPE (a whiteout is a char device 0,0; a placeholder is `d`), and
# `getfattr -d -m -` shows the overlayfs xattrs (trusted.overlay.opaque /
# .origin / .redirect) that would be copied — or silently dropped — by a
# byte-for-byte layer copy.
#
# It also SNAPSHOTS the shape into $WORK/tree.<probe> (type + upper-relative
# path, one entry per line) and the Volume's into $WORK/vol.<probe>. The VERDICT
# block asserts on those snapshots, never on the live filesystem: later probes
# WIPE the upper (reset_overlay), so an end-of-run `test -e` would silently
# report "no artifact" for every earlier probe and turn the whole measurement
# into a false negative.
dump_upper() {
  local probe="$1" label="$2"
  echo "----- UPPER after $label -----"
  echo "\$ ls -laR $UPPER"
  ls -laR "$UPPER"
  echo "\$ find $UPPER -printf '%y %M %s %p\\n'"
  find "$UPPER" -printf '%y %M %s %p\n' | sort -k4
  echo "\$ getfattr -R -d -m - --absolute-names $UPPER"
  getfattr -R -d -m - --absolute-names "$UPPER" 2>&1 || true
  echo "----- end UPPER after $label -----"
  find "$UPPER" -printf "%y %p\n" | sed "s| $UPPER| |" | sort -k2 >"$WORK/tree.$probe"
  find "$VOL"   -printf "%y %p\n" | sed "s| $VOL| |"   | sort -k2 >"$WORK/vol.$probe"
  getfattr -R -d -m - --absolute-names "$UPPER" 2>/dev/null \
    | sed "s|$UPPER|<upper>|" >"$WORK/xattr.$probe" || true
}

# write_bundle <extra-mounts-json> <guest-script>
# The fixed mount set mirrors spike-volume-mount.sh (proc + bare tmpfs /dev +
# tmpfs /tmp); the extra JSON is the Volume bind under test.
write_bundle() {
  local extra="$1" guest="$2"
  sudo mkdir -p "$BUNDLE"
  sudo tee "$BUNDLE/config.json" >/dev/null <<JSON
{"ociVersion":"1.0.0",
 "process":{"terminal":false,"user":{"uid":0,"gid":0},
   "args":["/bin/sh","-c","$guest"],
   "env":["PATH=/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root"],"cwd":"/",
   "capabilities":{"bounding":["CAP_DAC_OVERRIDE"],"effective":["CAP_DAC_OVERRIDE"],"permitted":["CAP_DAC_OVERRIDE"]}},
 "root":{"path":"$MERGED","readonly":false},"hostname":"guest",
 "mounts":[{"destination":"/proc","type":"proc","source":"proc"},
   {"destination":"/dev","type":"tmpfs","source":"tmpfs","options":["nosuid","mode=755","size=65536k"]},
   {"destination":"/tmp","type":"tmpfs","source":"tmpfs","options":["nosuid","nodev","mode=1777"]}$extra],
 "linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON
}

# run_probe <cid-suffix> — foreground `runsc run`, pinned flags. Captures the
# exit code and stderr verbatim: a launch FAILURE is itself a valid answer to
# "who creates the destination", so the error text is a deliverable, not noise.
# Each probe's stdout/rc is kept in a per-probe file so the VERDICT block can
# assert on all four after the fact.
run_probe() {
  local suffix="$1"
  set +e
  sudo runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
    --overlay2=none --file-access=shared --systemd-cgroup=false \
    run --bundle="$BUNDLE" "${CID}${suffix}" \
    >"$WORK/stdout.$suffix" 2>"$WORK/stderr.$suffix"
  echo $? >"$WORK/rc.$suffix"
  set -e
  echo "--- guest stdout ---"; sudo cat "$WORK/stdout.$suffix" || true
  echo "--- guest stderr ---"; sudo cat "$WORK/stderr.$suffix" || true
  echo "--- runsc exit=$(cat "$WORK/rc.$suffix") ---"
}

# say <label> <command...> — run the command as the assertion; PASS/FAIL only,
# never fatal (a "FAIL" here is a real measurement, and later probes still run).
say() {
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "PASS  $label"; else echo "FAIL  $label"; FAIL=1; fi
}
# Assertion primitives, all reading the per-probe SNAPSHOTS (see dump_upper) so
# that a later probe wiping the upper cannot retroactively falsify an earlier
# observation. `-F -x` on the tree matchers keeps "d /srv" from also matching
# "d /srv/data"; the *_lacks form uses a plain substring match on purpose, so
# "/data/" catches anything at all beneath the placeholder.
has()        { grep -q "$2" "$WORK/stdout.$1"; }
tree_has()   { grep -qFx "$2" "$WORK/tree.$1"; }
tree_lacks() { ! grep -qF "$2" "$WORK/tree.$1"; }
vol_has()    { grep -qFx "$2" "$WORK/vol.$1"; }

# ---------------------------------------------------------------- setup ------

echo "== build the Template (lower): dash + loader, no coreutils =="
"$REPO/test/preflight/build-rootfs.sh" "$LOWER" /usr/bin/dash >/dev/null
echo "-- lower top level (note: NO /data, NO /srv; /home DOES exist) --"
ls -la "$LOWER"

echo
echo "== seed the host-side Volume source dir =="
mkdir -p "$VOL"
echo "HOST_WROTE_THIS" > "$VOL/from_host"
sync
VOL_REAL="$(readlink -f "$VOL")"
ls -la "$VOL"

echo
echo "== assemble the host kernel overlayfs (engine option shape) =="
mount_overlay
echo "-- confirm merged is a real overlay mount --"
findmnt -no FSTYPE,SOURCE,TARGET --target "$MERGED"
echo "-- merged sees the Template through the (empty) upper --"
ls "$MERGED" | tr '\n' ' '; echo
dump_upper p0 "mount, before any probe (control: must be empty)"

# ================================================================ P1 =========
# WHY: the load-bearing case. Destination /data is in NEITHER lower nor upper.
# Two questions in one launch: (a) does launch fail or does something create the
# dir, and (b) where do guest writes under /data actually go — Volume or upper?
# /rootfs_control is a CONTROL write to an ordinary rootfs path: it proves the
# upper is live and does capture normal writes, so an EMPTY /data in the upper
# cannot be dismissed as "the upper wasn't recording".
echo
echo "############ P1: bind dest /data absent from lower AND upper ############"
P1_GUEST='echo P1_START; if [ -d /data ]; then echo DATA_IS_DIR=yes; else echo DATA_IS_DIR=no; fi; echo GLOB_START; for f in /data/*; do if [ -e $f ]; then echo GOT:$f; fi; done; echo GLOB_END; if [ -f /data/from_host ]; then read h < /data/from_host; echo HOST_CONTENT=$h; else echo HOST_CONTENT=MISSING; fi; if echo GUEST_WROTE_VOL > /data/from_guest; then echo VOL_WRITE=ok; else echo VOL_WRITE=fail; fi; if echo GUEST_WROTE_UPPER > /rootfs_control; then echo UPPER_WRITE=ok; else echo UPPER_WRITE=fail; fi; echo P1_DONE'
write_bundle ",
   {\"destination\":\"/data\",\"type\":\"bind\",\"source\":\"$VOL_REAL\",\"options\":[\"rbind\",\"rw\"]}" "$P1_GUEST"
run_probe p1
echo "-- did /data appear in the MERGED view? (merged = lower ∪ upper, and lower has none) --"
sudo ls -lad "$MERGED/data" 2>&1 || true
dump_upper p1 "P1 (/data bind, absent dest)"
echo "-- where did the guest's /data write land? --"
echo "\$ ls -la $VOL   (the Volume — writes SHOULD be here)"
sudo ls -la "$VOL"
echo "\$ ls -la $UPPER/data   (the Overlay upper — writes MUST NOT be here)"
sudo ls -la "$UPPER/data" 2>&1 || true

# ================================================================ P2 =========
# WHY: whatever P1 left in the upper is exactly what PromoteToTemplate would
# freeze. Relaunching on the SAME upper with NO bind shows a child sandbox's
# view of that frozen artifact: an empty placeholder dir, or nothing at all.
echo
echo "############ P2: SAME upper, relaunch WITHOUT the bind ############"
P2_GUEST='echo P2_START; if [ -d /data ]; then echo DATA_IS_DIR=yes; else echo DATA_IS_DIR=no; fi; if [ -e /data ]; then echo DATA_EXISTS=yes; else echo DATA_EXISTS=no; fi; echo GLOB_START; for f in /data/*; do if [ -e $f ]; then echo GOT:$f; fi; done; echo GLOB_END; if [ -f /rootfs_control ]; then read c < /rootfs_control; echo CONTROL=$c; else echo CONTROL=MISSING; fi; echo P2_DONE'
# fresh workdir, same upper (this is the engine's resume shape, §4.2)
umount_overlay
mount_overlay
write_bundle "" "$P2_GUEST"
run_probe p2
dump_upper p2 "P2 (no bind, same upper)"

# ================================================================ P3 =========
# WHY: if binding onto a dir that already exists in the Template forces a
# copy-up, then the upper is dirtied by the mere presence of a Volume even when
# the Template provides the mount point — which would make the excluded-paths
# entry mandatory rather than merely tidy. /home exists in the lower (see the
# listing above) and not in the upper.
echo
echo "############ P3: bind dest /home EXISTS in lower, not in upper ############"
reset_overlay
echo "-- pre-P3 control: /home in lower, absent from upper --"
sudo ls -lad "$LOWER/home"; sudo ls -la "$UPPER"
P3_GUEST='echo P3_START; if [ -d /home ]; then echo HOME_IS_DIR=yes; else echo HOME_IS_DIR=no; fi; echo GLOB_START; for f in /home/*; do if [ -e $f ]; then echo GOT:$f; fi; done; echo GLOB_END; if [ -f /home/from_host ]; then read h < /home/from_host; echo HOST_CONTENT=$h; else echo HOST_CONTENT=MISSING; fi; if echo GUEST_WROTE_HOME > /home/from_guest_p3; then echo VOL_WRITE=ok; else echo VOL_WRITE=fail; fi; echo P3_DONE'
write_bundle ",
   {\"destination\":\"/home\",\"type\":\"bind\",\"source\":\"$VOL_REAL\",\"options\":[\"rbind\",\"rw\"]}" "$P3_GUEST"
run_probe p3
dump_upper p3 "P3 (/home bind, dest exists in lower)"

# ================================================================ P4 =========
# WHY: nested absent destination. If the whole chain is created, the Promote
# exclusion cannot be a single exact path per Volume mount point — it has to
# cover the intermediate components too, or the layer keeps a phantom /srv and
# /srv/data with only the leaf removed.
echo
echo "############ P4: NESTED absent dest /srv/data/vol1 ############"
reset_overlay
P4_GUEST='echo P4_START; if [ -d /srv ]; then echo SRV_IS_DIR=yes; else echo SRV_IS_DIR=no; fi; if [ -d /srv/data ]; then echo SRVDATA_IS_DIR=yes; else echo SRVDATA_IS_DIR=no; fi; if [ -d /srv/data/vol1 ]; then echo LEAF_IS_DIR=yes; else echo LEAF_IS_DIR=no; fi; echo GLOB_START; for f in /srv/data/vol1/*; do if [ -e $f ]; then echo GOT:$f; fi; done; echo GLOB_END; if [ -f /srv/data/vol1/from_host ]; then read h < /srv/data/vol1/from_host; echo HOST_CONTENT=$h; else echo HOST_CONTENT=MISSING; fi; echo P4_DONE'
write_bundle ",
   {\"destination\":\"/srv/data/vol1\",\"type\":\"bind\",\"source\":\"$VOL_REAL\",\"options\":[\"rbind\",\"rw\"]}" "$P4_GUEST"
run_probe p4
echo "-- did the chain appear in MERGED? --"
sudo ls -lad "$MERGED/srv" "$MERGED/srv/data" "$MERGED/srv/data/vol1" 2>&1 || true
dump_upper p4 "P4 (nested /srv/data/vol1 bind)"

# ================================================================ P5 =========
# WHY: attribution. P1-P4 all RAN a guest, so "the guest touched /data" is not
# excluded as the cause. `runsc create` performs the full sandbox setup —
# including the mount table — and stops before the entrypoint runs, so anything
# in the upper at that point was materialized by the PLACEMENT, not the payload.
# That decides whether the Promote exclusion is unconditional (every placement
# with a Volume) or contingent on guest behaviour.
#
# The Volume source is chmod 0711 here: if the placeholder inherited the source
# mode, the layer would freeze the Volume's permissions onto an unrelated
# rootfs dir. `sleep`-free entrypoint: we never start it, so the args only need
# to exist (/bin/sh is enough).
echo
echo '############ P5: runsc create ONLY — attribution + placeholder mode ############'
reset_overlay
chmod 0711 "$VOL"
echo "-- Volume source mode is now: $(stat -c '%a %n' "$VOL") --"
write_bundle ",
   {\"destination\":\"/data\",\"type\":\"bind\",\"source\":\"$VOL_REAL\",\"options\":[\"rbind\",\"rw\"]}" 'echo NEVER_STARTED'
set +e
sudo runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
  --overlay2=none --file-access=shared --systemd-cgroup=false \
  create --bundle="$BUNDLE" "${CID}p5" >"$WORK/stdout.p5" 2>"$WORK/stderr.p5"
echo $? >"$WORK/rc.p5"
set -e
echo "--- create stdout ---"; cat "$WORK/stdout.p5" || true
echo "--- create stderr ---"; cat "$WORK/stderr.p5" || true
echo "--- runsc create exit=$(cat "$WORK/rc.p5") ---"
echo "-- container state (must be 'created', NOT 'running') --"
sudo runsc --root="$RUNSC_ROOT" state "${CID}p5" 2>&1 | grep -E '"status"|"id"' || true
echo "-- placeholder mode in the upper, if any (this is what Promote would freeze) --"
stat -c '%a' "$UPPER/data" >"$WORK/mode.p5" 2>/dev/null || echo none >"$WORK/mode.p5"
stat -c '%A %a %U:%G %n' "$UPPER/data" 2>&1 || true
dump_upper p5 "P5 (created, never started)"
sudo runsc --root="$RUNSC_ROOT" delete --force "${CID}p5" 2>/dev/null || true

# ================================================================ P6 =========
# WHY: the mount point may already hold BYTES in the upper before the Volume is
# bound — §5.C's own rule is that mount-table changes take effect at the resume
# boundary, so a sandbox can run once without the Volume (guest writes /data/x),
# then resume WITH it. The bind now shadows /data/x: the guest cannot see it, but
# it is still on disk in the upper, still charged to the sandbox's §3.4 cap, and
# still walked by copyTree. This is the case that decides whether the Promote
# exclusion must be a SUBTREE prune rather than "skip the empty dir".
echo
echo "############ P6: upper already holds SHADOWED bytes at the mount point ############"
reset_overlay
chmod 0755 "$VOL"
echo "-- seed the upper directly, as an earlier no-Volume placement would have --"
sudo mkdir -p "$UPPER/data"
echo "STALE_UPPER_BYTES" | sudo tee "$UPPER/data/stale" >/dev/null
umount_overlay; mount_overlay   # remount so the seeded upper is in the merged view
echo "-- pre-P6 upper (the bytes about to be shadowed) --"
find "$UPPER" -printf '%y %s %p\n' | sort -k3
P6_GUEST='echo P6_START; echo GLOB_START; for f in /data/*; do if [ -e $f ]; then echo GOT:$f; fi; done; echo GLOB_END; if [ -f /data/stale ]; then echo STALE_VISIBLE=yes; else echo STALE_VISIBLE=no; fi; if [ -f /data/from_host ]; then echo VOLUME_VISIBLE=yes; else echo VOLUME_VISIBLE=no; fi; echo P6_DONE'
write_bundle ",
   {\"destination\":\"/data\",\"type\":\"bind\",\"source\":\"$VOL_REAL\",\"options\":[\"rbind\",\"rw\"]}" "$P6_GUEST"
run_probe p6
dump_upper p6 "P6 (bind shadowing pre-existing upper bytes)"

# ============================================================== VERDICT ======
echo
echo "==================== VERDICT ===================="
echo "-- exit codes: p1=$(cat "$WORK/rc.p1") p2=$(cat "$WORK/rc.p2") p3=$(cat "$WORK/rc.p3") p4=$(cat "$WORK/rc.p4") p5(create)=$(cat "$WORK/rc.p5") p6=$(cat "$WORK/rc.p6")"
echo
echo "-- upper shape snapshots (type + upper-relative path), one per probe --"
for p in p0 p1 p2 p3 p4 p5 p6; do
  echo "  [$p] $(tr '\n' '|' <"$WORK/tree.$p")"
done
echo "-- overlayfs xattrs seen anywhere in the upper, across all probes --"
grep -h '=' "$WORK"/xattr.p? | sort -u | sed 's/^/  /'

echo
echo "[Q1/Q2] absent dest /data: does launch fail, or is the destination created?"
say  "P1 launch SUCCEEDED (rc=0) over a merged host overlayfs"  test "$(cat "$WORK/rc.p1")" = 0
say  "P1 guest ran to completion"                               has p1 P1_DONE
say  "P1 guest sees the absent dest /data as a directory"        has p1 DATA_IS_DIR=yes
say  "P1 guest reads Volume content through the absent dest"     has p1 HOST_CONTENT=HOST_WROTE_THIS
say  "P1 the dest materialized as a DIRECTORY in the UPPER"      tree_has p1 "d /data"
say  "P1 that upper entry is NOT a whiteout (no char dev)"        tree_lacks p1 "c /data"
say  "P1 the upper's /data placeholder is EMPTY (no Volume content captured)" \
     tree_lacks p1 "/data/"
say  "P1 the placeholder carries NO overlayfs xattrs (opaque/origin/redirect)" \
     sh -c "! grep -q 'trusted.overlay.\(opaque\|origin\|redirect\)' '$WORK/xattr.p1'"

echo
echo "[Q6] reverse hazard: guest writes under the bind → Volume, never the upper"
say  "P1 guest write to /data succeeded"                         has p1 VOL_WRITE=ok
say  "P1 that write reached the VOLUME host dir"                 vol_has p1 "f /from_guest"
say  "P1 that write did NOT leak into the Overlay upper"          tree_lacks p1 "/data/from_guest"
say  "P1 control: an ordinary rootfs write DID reach the upper (proves the upper was live)" \
     tree_has p1 "f /rootfs_control"

echo
echo "[Q7] relaunch on the SAME upper WITHOUT the bind (a child's view of the artifact)"
say  "P2 launch succeeded"                                       test "$(cat "$WORK/rc.p2")" = 0
say  "P2 control file survived in the upper (same artifact, remounted)" has p2 CONTROL=GUEST_WROTE_UPPER
say  "P2 the guest STILL sees /data — a durable phantom placeholder"     has p2 DATA_EXISTS=yes
say  "P2 that /data is EMPTY: no Volume content visible without the bind" \
     sh -c "! grep -q 'GOT:/data' '$WORK/stdout.p2'"

echo
echo "[Q3] dest EXISTS in the lower (Template): does binding force a copy-up?"
say  "P3 launch succeeded"                                       test "$(cat "$WORK/rc.p3")" = 0
say  "P3 guest reads Volume content at /home"                    has p3 HOST_CONTENT=HOST_WROTE_THIS
say  "P3 guest write under the bind reached the Volume"          vol_has p3 "f /from_guest_p3"
say  "P3 NO /home copy-up in the upper — the upper stayed CLEAN" tree_lacks p3 "/home"

echo
echo "[Q4] NESTED absent dest /srv/data/vol1 (no component exists)"
say  "P4 launch succeeded"                                       test "$(cat "$WORK/rc.p4")" = 0
say  "P4 guest sees the whole chain and the Volume content"      has p4 HOST_CONTENT=HOST_WROTE_THIS
say  "P4 the WHOLE chain landed in the upper: /srv"              tree_has p4 "d /srv"
say  "P4 …intermediate /srv/data"                                tree_has p4 "d /srv/data"
say  "P4 …leaf /srv/data/vol1"                                   tree_has p4 "d /srv/data/vol1"
say  "P4 nothing below the leaf (no Volume content captured)"     tree_lacks p4 "/srv/data/vol1/"

echo
echo "[attribution] created-but-never-started: placement or payload?"
say  "P5 runsc create succeeded"                                 test "$(cat "$WORK/rc.p5")" = 0
say  "P5 the placeholder exists in the upper with NO guest code ever run" \
     tree_has p5 "d /data"
say  "P5 nothing else was written (only the mount point)"         tree_lacks p5 "/rootfs_control"
say  "P5 the placeholder did NOT inherit the Volume source's 0711 mode (its mode is $(cat "$WORK/mode.p5"))" \
     sh -c "[ \"\$(cat '$WORK/mode.p5')\" != 711 ]"

echo
echo "[shadowing] pre-existing upper bytes under a bound mount point"
say  "P6 launch succeeded"                                       test "$(cat "$WORK/rc.p6")" = 0
say  "P6 the guest sees the VOLUME at /data"                     has p6 VOLUME_VISIBLE=yes
say  "P6 the guest CANNOT see the shadowed upper bytes"          has p6 STALE_VISIBLE=no
say  "P6 yet those bytes are STILL in the upper after the run"    tree_has p6 "f /data/stale"

echo
echo "[Q5] Promote capture — reasoned against internal/rootfs/overlayfs.go:"
echo "  copyTree walks the upper and materializes dirs with MkdirAll, so the"
echo "  empty placeholder directories measured above ARE captured byte-for-byte"
echo "  into the new layer. A Template promoted from a sandbox that ever had a"
echo "  Volume therefore carries a PHANTOM empty mount-point tree — for a nested"
echo "  destination, every component of it. Nothing is a whiteout, so the"
echo "  non-regular-node skip in copyTree does not save us, and no overlayfs"
echo "  xattrs are involved. P6 adds the sharper case: shadowed bytes under the"
echo "  mount point are invisible to the guest but fully copied, so the exclusion"
echo "  must PRUNE THE SUBTREE at each bound destination, not just skip an empty"
echo "  dir — and it must be derived from the live mount table, since P5 proves"
echo "  the placeholder is created by the PLACEMENT and not by guest activity."
echo
echo "FAIL count: $FAIL"
exit $FAIL
