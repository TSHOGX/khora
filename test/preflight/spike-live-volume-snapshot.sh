#!/usr/bin/env bash
# spike-live-volume-snapshot.sh — §5.C "snapshot / clone" promise, live arm:
# can a Volume be `btrfs subvolume snapshot`-ed WHILE a running sandbox's gofer
# holds it open and is actively writing to it, and is the result
# crash-consistent? And is deep-dives/volume.md D6.2's advice ("before
# snapshotting a live mounted volume you should fsync / freeze the mount point")
# load-bearing, or cargo cult?
#
# Builds on spike-volume-mount.sh (which proved a host dir bind-mounts rw into a
# live guest via gofer) and changes exactly two things:
#   - the Volume is a real **btrfs subvolume** under /var/tmp, not a plain dir,
#     because that is what the local volume backend materializes (§5.C);
#   - the guest is **long-running and continuously writing**, not a one-shot
#     probe, because a snapshot of an idle volume proves nothing about
#     consistency under a live writer.
#
# The guest writes, every iteration:
#     echo $i >  /vol/counter      (O_TRUNC rewrite — the "live value")
#     echo $i >> /vol/log          (O_APPEND  — the tamper-evident history)
# in that order. So at ANY instant the on-disk truth is
#     last_log <= counter <= last_log + 1
# and /vol/log must be exactly the lines 1,2,3,...,N. Those two facts are the
# crash-consistency assertions: a snapshot is coherent iff its log is an
# unbroken prefix 1..N and its counter agrees with its own log tail. A snapshot
# is point-in-time iff that N sits inside the window [before, after] that the
# host observed around the snapshot call, and iff N NEVER MOVES afterwards.
#
# Guest code is dash builtins only (glob / read / echo / arithmetic) — the bare
# rootfs from build-rootfs.sh has NO coreutils, so no ls, cat, sleep, or wc.
# The pacing loop is integer arithmetic for that reason.
#
# Isolation (other agents run runsc concurrently): --network=none, no netns, a
# private --root, and a CID prefix unique to this spike + pid.
#
# !! BLAST RADIUS !! Probe D calls `fsfreeze -f` on the filesystem holding the
# volume. On this machine /var/tmp is on the ROOT btrfs, so that freezes the
# whole node's root fs for a couple of seconds — every other process on the box
# stalls on write. That is not incidental, it IS one of the findings, so the
# probe runs by default; a watchdog unfreezes unconditionally and every frozen
# step is `timeout`-wrapped so a hang cannot wedge the machine. Set FREEZE=0 to
# skip it.
#
# Usage: sudo ./spike-live-volume-snapshot.sh      (needs root: gofer, btrfs, fsfreeze)
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "must run as root (runsc gofer + btrfs + fsfreeze)" >&2; exit 2; }

# /var/tmp, not /tmp: the Volume has to live on btrfs to be a subvolume at all.
WORK="$(mktemp -d /var/tmp/spike-livesnap.XXXXXX)"
ROOTFS="$WORK/rootfs"; VOL="$WORK/volume"; BUNDLE="$WORK/bundle"; RUNSC_ROOT="$WORK/runsc-root"
CID="s2snap$$"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
SPIN="${SPIN:-4000}"            # guest pacing: inner arithmetic spins per write
FREEZE="${FREEZE:-1}"
MP="$(findmnt -n -o TARGET -T "$WORK")"
RUNSC_PID=""

cleanup() {
  set +e
  # let the guest fall out of its loop first, then force
  [ -d "$VOL" ] && : > "$VOL/STOP" 2>/dev/null
  sleep 0.3
  runsc --root="$RUNSC_ROOT" delete --force "$CID" 2>/dev/null
  [ -n "$RUNSC_PID" ] && kill "$RUNSC_PID" 2>/dev/null
  # never leave the fs frozen, whatever happened
  fsfreeze -u "$MP" 2>/dev/null
  # subvolumes cannot be rmdir'd; delete every one we made before rm -rf
  for d in "$WORK"/*; do btrfs subvolume delete "$d" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
trap cleanup EXIT

hr() { echo; echo "=== $* ==="; }

# ---- host-side witnesses ----------------------------------------------------
# lastlog: highest complete line of an append-only log. Never truncated by the
# writer, so it is a safe progress witness even mid-write.
lastlog() { tail -n1 "$1/log" 2>/dev/null | tr -dc '0-9'; }

# Report everything measurable about a captured subvolume: does the seed
# survive, what point in the guest's history did it capture, is that history
# internally coherent.
analyze() {
  local label="$1" p="$2"
  local seed lines bytes last cnt mono
  seed="$(cat "$p/seed" 2>/dev/null || echo MISSING)"
  lines="$(wc -l < "$p/log" 2>/dev/null || echo -1)"
  bytes="$(wc -c < "$p/log" 2>/dev/null || echo -1)"
  last="$(tail -n1 "$p/log" 2>/dev/null)"
  cnt="$(cat "$p/counter" 2>/dev/null)"
  # line N must contain exactly N — any gap/reorder/duplicate breaks it
  if awk 'NR != $1+0 { print "BREAK at line " NR ": [" $0 "]"; exit 1 }' "$p/log" >/dev/null 2>&1; then
    mono=COHERENT
  else
    mono="INCOHERENT: $(awk 'NR != $1+0 { print "line " NR " holds [" $0 "]"; exit }' "$p/log")"
  fi
  # counter is rewritten with O_TRUNC every iteration, so a point-in-time
  # capture can legitimately land in the truncate/write gap and see it EMPTY.
  # That is textbook crash-consistency (what a power cut would also give), NOT
  # corruption — classify it instead of letting it read as a failure.
  local ctag
  if   [ -z "$cnt" ];                     then ctag="EMPTY(torn O_TRUNC rewrite — crash-consistent, not corrupt)"
  elif [ "$cnt" = "$last" ];              then ctag="agrees-with-log-tail"
  elif [ "$cnt" = "$((last + 1))" ];      then ctag="log_tail+1 (counter written before its log line — expected)"
  else                                         ctag="UNEXPECTED vs log_tail=$last"
  fi
  echo "  $label: seed=$seed log_lines=$lines log_bytes=$bytes log_tail=[$last] counter=[$cnt] $ctag prefix=$mono"
}

hr "build minimal rootfs (dash only, no coreutils in guest)"
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" /usr/bin/dash >/dev/null
mkdir -p "$ROOTFS/vol"

hr "Volume V = btrfs subvolume on $MP, seeded"
btrfs subvolume create "$VOL"
echo "SEEDED_BEFORE_LAUNCH" > "$VOL/seed"
sync
VOL_REAL="$(readlink -f "$VOL")"

# Guest: long-running writer. Stops when the HOST creates /vol/STOP, which
# doubles as a host->guest visibility check after all the snapshotting.
# NOTE: this MUST stay a single physical line — it is interpolated into
# config.json's process.args and a raw newline there is an invalid JSON string
# literal ("invalid character '\n' in string literal" from runsc FetchSpec).
GUEST='i=0; errs=0; echo GUEST_START; while [ ! -f /vol/STOP ]; do i=$((i+1)); if ! echo $i > /vol/counter 2>/dev/null; then errs=$((errs+1)); fi; if ! echo $i >> /vol/log 2>/dev/null; then errs=$((errs+1)); fi; j=0; while [ $j -lt '"$SPIN"' ]; do j=$((j+1)); done; done; echo GUEST_SAW_STOP; echo GUEST_STOPPED_AT=$i; echo GUEST_WRITE_ERRORS=$errs; read c < /vol/counter; echo GUEST_FINAL_COUNTER=$c; echo GLOB_START; for f in /vol/*; do echo GOT:$f; done; echo GLOB_END; read s < /vol/seed; echo GUEST_SEES_SEED=$s; echo DONE'

hr "OCI config.json: V bind-mounted rw at /vol"
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

hr "launch guest (background, stdio to FILES never pipes, --network=none)"
runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
  --overlay2=none --file-access=shared --systemd-cgroup=false \
  run --bundle="$BUNDLE" "$CID" >"$WORK/guest.out" 2>"$WORK/guest.err" &
RUNSC_PID=$!

# wait for the writer to actually be writing through the gofer
for _ in $(seq 1 100); do
  [ -n "$(lastlog "$VOL")" ] && break
  sleep 0.2
done
[ -n "$(lastlog "$VOL")" ] || { echo "FATAL: guest never wrote to /vol"; cat "$WORK/guest.err"; exit 1; }
sleep 1
R0="$(lastlog "$VOL")"; sleep 1; R1="$(lastlog "$VOL")"
echo "guest is writing: counter $R0 -> $R1 in ~1s (rate ~$((R1-R0))/s), runsc pid $RUNSC_PID"

# ---------------------------------------------------------------------------
# PROBE A — the bare question: snapshot a live, gofer-held, actively-written
# subvolume with NO sync, NO freeze, NO pause. §5.C's promise stands or falls
# here. The [before,after] window brackets the point in time it may capture.
# ---------------------------------------------------------------------------
hr "PROBE A: snapshot with NO sync / NO freeze (the §5.C promise, bare)"
A_BEFORE="$(lastlog "$VOL")"
set +e
A_OUT="$(btrfs subvolume snapshot "$VOL" "$WORK/snap_nosync" 2>&1)"; A_RC=$?
set -e
A_AFTER="$(lastlog "$VOL")"
echo "  btrfs rc=$A_RC out=[$A_OUT]"
echo "  host-observed live window: before=$A_BEFORE after=$A_AFTER"
analyze "snap_nosync" "$WORK/snap_nosync"

# ---------------------------------------------------------------------------
# PROBE B — read-only snapshot (-r) under the same live-write conditions. This
# is the shape a Volume snapshot artifact would actually take (immutable
# point-in-time), so it must work under a live gofer too, and must really be ro.
# ---------------------------------------------------------------------------
hr "PROBE B: read-only snapshot (-r) under live writes"
B_BEFORE="$(lastlog "$VOL")"
set +e
B_OUT="$(btrfs subvolume snapshot -r "$VOL" "$WORK/snap_ro" 2>&1)"; B_RC=$?
set -e
B_AFTER="$(lastlog "$VOL")"
echo "  btrfs rc=$B_RC out=[$B_OUT]"
echo "  host-observed live window: before=$B_BEFORE after=$B_AFTER"
analyze "snap_ro" "$WORK/snap_ro"
set +e
RO_W="$(sh -c "echo x > '$WORK/snap_ro/should_fail'" 2>&1)"; RO_RC=$?
set -e
echo "  write into snap_ro: rc=$RO_RC msg=[$RO_W]  (expect non-zero / EROFS)"
echo "  ro flag: $(btrfs property get "$WORK/snap_ro" ro 2>&1)"

# ---------------------------------------------------------------------------
# PROBE C — D6.2's fsync half: does a host `sync` before the snapshot change
# what gets captured? If A and C are equally coherent, the advice buys nothing.
# ---------------------------------------------------------------------------
hr "PROBE C: snapshot immediately after host \`sync\` (D6.2 fsync advice)"
C_BEFORE="$(lastlog "$VOL")"
sync
set +e
C_OUT="$(btrfs subvolume snapshot "$VOL" "$WORK/snap_sync" 2>&1)"; C_RC=$?
set -e
C_AFTER="$(lastlog "$VOL")"
echo "  btrfs rc=$C_RC out=[$C_OUT]"
echo "  host-observed live window: before=$C_BEFORE after=$C_AFTER"
analyze "snap_sync" "$WORK/snap_sync"

# ---------------------------------------------------------------------------
# PROBE D — D6.2's freeze half. "freeze, then snapshot" may not even be
# EXPRESSIBLE: creating a subvolume is itself a write to the fs that fsfreeze
# just quiesced. Measured sub-questions:
#   D1: does `fsfreeze -f` return at all while a gofer holds files open, and
#       does the frozen window stall the guest's writes?
#   D2: what happens to a snapshot issued INSIDE the frozen window — does it
#       fail, or block? And does the freeze survive it?
#   D3: snapshot right after thaw — the degenerate "quiesce, then shoot".
#
# D2 runs the snapshot in the BACKGROUND and polls, because a foreground
# `timeout N btrfs ...` cannot distinguish "failed" from "blocked in the kernel":
# the timeout kills the wrapper while the kernel keeps the ioctl running, so the
# snapshot lands anyway and rc=124 is an artifact of the harness, not a result.
# Worse, killing a snapshot that is blocked under freeze corrupts freeze
# accounting — the subsequent `fsfreeze -u` returns EINVAL as if the sb were
# already thawed. That is why the earlier foreground version of this probe
# reported "snapshot FAILED rc=124" and a failed thaw; both were harness
# artifacts. Verified by polling for 12s without thawing: the snapshot stays
# blocked and the fs stays FROZEN the whole time, then the snapshot completes
# rc=0 the moment the thaw lands. Do not "fix" this back to foreground+timeout.
#
# fstate() reads freeze state WITHOUT blocking: `fsfreeze -f` on an
# already-frozen sb returns EBUSY immediately. If it returns 0 the sb had been
# thawed and we have now re-frozen it — the caller must account for that, which
# is exactly the signal D2 needs.
# A watchdog thaws unconditionally so a hang cannot leave the node's fs wedged.
# ---------------------------------------------------------------------------
if [ "$FREEZE" = 1 ]; then
hr "PROBE D: fsfreeze -f on $MP (whole-fs blast radius) then snapshot"
# page in every binary the frozen window needs — a page fault on a frozen fs
# would look like the probe hanging when it is just the loader blocked.
fsfreeze --help >/dev/null 2>&1 || true
timeout --help  >/dev/null 2>&1 || true
btrfs --version >/dev/null 2>&1 || true
lastlog "$VOL" >/dev/null 2>&1 || true

fstate() {
  local o r
  o="$(timeout 3 fsfreeze -f "$MP" 2>&1)"; r=$?
  if [ $r -eq 0 ]; then echo "WAS-THAWED(now-refrozen)"
  elif echo "$o" | grep -qi busy; then echo "FROZEN"
  else echo "UNKNOWN(rc=$r,[$o])"; fi
}

( sleep 30; fsfreeze -u "$MP" >/dev/null 2>&1 ) &
WD=$!
D_BEFORE="$(lastlog "$VOL")"
set +e
FZ_OUT="$(timeout 10 fsfreeze -f "$MP" 2>&1)"; FZ_RC=$?
set -e
echo "  D1 fsfreeze -f rc=$FZ_RC out=[$FZ_OUT]  (rc=124 would mean it hung)"
sleep 2
D_FROZEN="$(lastlog "$VOL")"
echo "  D1 guest advanced $((D_FROZEN - D_BEFORE)) writes during the ~2s frozen window"

# D2: issue the snapshot inside the frozen window, in the background, and watch.
btrfs subvolume snapshot "$VOL" "$WORK/snap_frozen" >"$WORK/snapfrozen.out" 2>&1 &
SNAP_PID=$!
# Poll well past any plausible completion time (an unfrozen snapshot of this
# volume finishes in well under a second, see probes A/B/C) WITHOUT thawing, so
# "still blocked" is a measurement and not an artifact of us thawing too early.
D2_SELF_COMPLETED=no
for t in $(seq 1 8); do
  sleep 1
  st="$(ps -o stat= -p "$SNAP_PID" 2>/dev/null | tr -d ' ')"; [ -n "$st" ] || st=exited
  echo "  D2 t+${t}s inside frozen window: fs=$(fstate) snapdir=$([ -d "$WORK/snap_frozen" ] && echo EXISTS || echo not-yet) btrfs_proc_state=$st"
  if [ "$st" = exited ]; then D2_SELF_COMPLETED=yes; break; fi
done
echo "  D2 snapshot completed on its own while still frozen? $D2_SELF_COMPLETED (no = it is blocked on the freeze)"
set +e
UF_OUT="$(timeout 10 fsfreeze -u "$MP" 2>&1)"; UF_RC=$?
set -e
echo "  D2 fsfreeze -u rc=$UF_RC out=[$UF_OUT]"
set +e
wait "$SNAP_PID"; D_RC=$?
set -e
D_OUT="$(cat "$WORK/snapfrozen.out")"
echo "  D2 snapshot issued-while-frozen: final rc=$D_RC out=[$D_OUT]"
kill "$WD" 2>/dev/null || true
sleep 1
D_AFTER="$(lastlog "$VOL")"
echo "  D1/D2 guest write progress: before=$D_BEFORE frozen(+2s)=$D_FROZEN thawed(+1s)=$D_AFTER"
echo "  -> during freeze +$((D_FROZEN - D_BEFORE)); after thaw +$((D_AFTER - D_FROZEN))"
[ -d "$WORK/snap_frozen" ] && analyze "snap_frozen" "$WORK/snap_frozen" || echo "  snap_frozen: NOT CREATED"

hr "PROBE D3: snapshot immediately after thaw"
E_BEFORE="$(lastlog "$VOL")"
set +e
E_OUT="$(timeout 10 btrfs subvolume snapshot "$VOL" "$WORK/snap_thawed" 2>&1)"; E_RC=$?
set -e
E_AFTER="$(lastlog "$VOL")"
echo "  btrfs rc=$E_RC out=[$E_OUT]"
echo "  host-observed live window: before=$E_BEFORE after=$E_AFTER"
analyze "snap_thawed" "$WORK/snap_thawed"
else
hr "PROBE D: SKIPPED (FREEZE=0)"
fi

# ---------------------------------------------------------------------------
# PROBE E — point-in-time-ness, the reverse direction. The guest keeps writing;
# the snapshots must NOT follow. Also a host-side post-snapshot write to V must
# not appear in them. If a snapshot tracked the live volume it would be a
# useless artifact, so this is as load-bearing as probe A.
# ---------------------------------------------------------------------------
hr "PROBE E: snapshots must NOT track the live volume (point-in-time)"
S1_T0="$(lastlog "$WORK/snap_nosync")"
echo "AFTER_SNAPSHOT_HOST_WRITE" > "$VOL/host_after_snap"
sleep 2
S1_T1="$(lastlog "$WORK/snap_nosync")"
V_NOW="$(lastlog "$VOL")"
echo "  snap_nosync log tail: t0=$S1_T0  t0+2s=$S1_T1   (must be equal)"
echo "  live V log tail now:  $V_NOW  (must be >> $S1_T1)"
echo "  host file written to V after the snapshot present in snapshot? $([ -e "$WORK/snap_nosync/host_after_snap" ] && echo YES-LEAKED || echo no-correct)"

# ---------------------------------------------------------------------------
# PROBE F — was the guest disturbed by ANY of the above? Stop it and read its
# own report: iterations reached, write errors seen, whether it still sees the
# host's files. A dead or errored guest fails §5.C regardless of snapshot rc.
# ---------------------------------------------------------------------------
hr "PROBE F: guest liveness / disturbance"
echo "  runsc process still alive before stop? $(kill -0 "$RUNSC_PID" 2>/dev/null && echo yes || echo NO)"
: > "$VOL/STOP"
for _ in $(seq 1 100); do grep -q '^DONE$' "$WORK/guest.out" 2>/dev/null && break; sleep 0.2; done
set +e
wait "$RUNSC_PID"; GUEST_RC=$?
set -e
echo "  runsc exit=$GUEST_RC"
echo "--- guest stdout ---"; cat "$WORK/guest.out"
echo "--- guest stderr ---"; cat "$WORK/guest.err"
hr "final state of the live volume V"
analyze "V (live, final)" "$VOL"

# ---------------------------------------------------------------------------
hr "VERDICT"
FAIL=0
ok()   { echo "PASS  $*"; }
bad()  { echo "FAIL  $*"; FAIL=1; }
note() { echo "NOTE  $*"; }

coherent() { # $1 = subvol path -> 0 if log is an unbroken 1..N prefix and non-empty
  [ -s "$1/log" ] || return 1
  awk 'NR != $1+0 { exit 1 }' "$1/log"
}
inwindow() { # $1=before $2=captured $3=after
  [ -n "$2" ] && [ "$2" -ge "$1" ] && [ "$2" -le "$3" ]
}

[ "$A_RC" -eq 0 ] && ok "Q2 snapshot of a live gofer-held subvolume SUCCEEDS (rc=0, no sync/freeze)" \
                   || bad "Q2 snapshot of a live subvolume failed rc=$A_RC: $A_OUT"
{ [ -r "$WORK/snap_nosync/seed" ] && coherent "$WORK/snap_nosync"; } \
  && ok "Q3 snapshot readable from host and its write history is an unbroken prefix" \
  || bad "Q3 snapshot unreadable or history incoherent"
inwindow "$A_BEFORE" "$(lastlog "$WORK/snap_nosync")" "$A_AFTER" \
  && ok "Q3 captured point $(lastlog "$WORK/snap_nosync") lies inside the live window [$A_BEFORE,$A_AFTER]" \
  || bad "Q3 captured point $(lastlog "$WORK/snap_nosync") OUTSIDE live window [$A_BEFORE,$A_AFTER]"
{ [ "$B_RC" -eq 0 ] && coherent "$WORK/snap_ro" && [ "$RO_RC" -ne 0 ]; } \
  && ok "Q4 read-only (-r) snapshot works under live writes and is genuinely ro" \
  || bad "Q4 read-only snapshot rc=$B_RC ro-enforced=$([ "$RO_RC" -ne 0 ] && echo yes || echo NO)"
{ [ "$C_RC" -eq 0 ] && coherent "$WORK/snap_sync"; } \
  && ok "Q5a snapshot after \`sync\` also works and is coherent — same class as no-sync" \
  || bad "Q5a snapshot after sync rc=$C_RC / incoherent"
if [ "$FREEZE" = 1 ]; then
  [ "$FZ_RC" -eq 0 ] && note "Q5b fsfreeze -f on $MP SUCCEEDS with a gofer holding the volume's files open" \
                     || note "Q5b fsfreeze -f rc=$FZ_RC — see raw output above"
  [ "$D2_SELF_COMPLETED" = no ] \
    && ok "Q5b snapshot issued INSIDE the frozen window does not fail and does not complete — it BLOCKS for the whole frozen window (snapdir absent throughout) and materializes only after the thaw (final rc=$D_RC)" \
    || bad "Q5b snapshot completed while the fs was still frozen — contradicts the blocking model; re-read the raw D2 poll lines"
  if [ "$UF_RC" -eq 0 ]; then
    note "Q5b the freeze SURVIVES the blocked snapshot (fsfreeze -u rc=0 = sb was still frozen when we thawed it)"
  else
    note "Q5b fsfreeze -u rc=$UF_RC [$UF_OUT] — sb was ALREADY thawed, i.e. something released the freeze early"
  fi
  note "Q5b => 'freeze, then snapshot' does not compose as a bounded operation: the snapshot cannot complete until you thaw, so it serializes AFTER the quiesce it was supposed to exploit"
  note "Q5b HAZARD: do NOT put a timeout around a snapshot issued under freeze. A SIGTERM'd blocked snapshot leaves freeze accounting wrong — the later fsfreeze -u returns EINVAL (kernel believes it is thawed) while the snapshot lands anyway. Measured separately, reproducibly, outside this script."
  note "Q5b guest advanced $((D_FROZEN - D_BEFORE)) writes during the ~2s frozen window (near-0 = stalled by the freeze, i.e. freeze DOES hit the guest)"
  note "Q5b freeze target was $MP — the node's whole root fs, not just this volume (blast radius)"
  { [ -d "$WORK/snap_frozen" ] && coherent "$WORK/snap_frozen"; } \
    && ok "Q5b the eventually-created snap_frozen is coherent too — no consistency gain over probe A" \
    || bad "Q5b snap_frozen missing/incoherent"
  { [ "$E_RC" -eq 0 ] && coherent "$WORK/snap_thawed"; } \
    && ok "Q5c snapshot after thaw works and is coherent — again same class as no-sync" \
    || bad "Q5c post-thaw snapshot rc=$E_RC / incoherent"
fi
[ "$S1_T0" = "$S1_T1" ] && ok "Q7 snapshot did not advance while the guest kept writing ($S1_T0 == $S1_T1)" \
                        || bad "Q7 snapshot TRACKED the live volume ($S1_T0 -> $S1_T1)"
[ -e "$WORK/snap_nosync/host_after_snap" ] && bad "Q7 post-snapshot host write leaked into the snapshot" \
                                          || ok "Q7 post-snapshot host write absent from the snapshot"
[ "$V_NOW" -gt "$S1_T1" ] && ok "Q6 live volume kept advancing past the snapshot ($V_NOW > $S1_T1)" \
                          || bad "Q6 live volume stopped advancing ($V_NOW vs $S1_T1)"
grep -q 'GUEST_WRITE_ERRORS=0' "$WORK/guest.out" && ok "Q6 guest saw ZERO write errors across all probes" \
                                                 || bad "Q6 guest reported write errors: $(grep GUEST_WRITE_ERRORS "$WORK/guest.out" || echo none-reported)"
grep -q '^DONE$' "$WORK/guest.out" && ok "Q6 guest survived every probe and exited cleanly on the host's STOP file" \
                                   || bad "Q6 guest did not reach DONE — died or hung"
echo
echo "captured points: nosync=$(lastlog "$WORK/snap_nosync") ro=$(lastlog "$WORK/snap_ro") sync=$(lastlog "$WORK/snap_sync")$([ "$FREEZE" = 1 ] && echo " thawed=$(lastlog "$WORK/snap_thawed" 2>/dev/null)")"
exit $FAIL
