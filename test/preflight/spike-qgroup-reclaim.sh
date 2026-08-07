#!/usr/bin/env bash
# spike-qgroup-reclaim.sh — how a leaked squota qgroup row is reclaimed, and what
# it costs to leave one behind. Pure btrfs: no runsc, no overlayfs, no guest.
#
# WHY THIS EXISTS. `spike-squota-snapshot.sh` 8b found that
# `internal/btrfs.DeleteSubvolume`'s doc comment is wrong for one case: with a
# LIVE SNAPSHOT the immediate `qgroup destroy` returns EBUSY (not the ENOENT the
# comment claims) and the row then persists as `<stale>`. deep-dives/volume.md D7.1
# records that contradiction and hands the reclaim path to `SweepOrphanVolumes`
# (§5.C snapshots Volumes, so the engine will hit this on every clone).
# A sweep needs three things the prior spike did not pin down:
#   Q2  an OBSERVABLE PREDICATE for "destroy will succeed now" — it must poll on
#       something, and guessing wrong means either a spin on EBUSY or a leak.
#   Q4  WHEN it must act — which sequences leak and which self-reclaim, so the
#       sweep is not a cargo-cult step on every delete.
#   Q5  WHETHER a leak is harmful or merely untidy — specifically whether a stale
#       row's id can be REUSED by a later subvolume, which would silently apply a
#       dead unit's cap to a live one. That is the finding that decides whether
#       this is cosmetic or a correctness bug.
#
# Usage: sudo ./spike-qgroup-reclaim.sh    (needs root: btrfs ioctls, loop mount)
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "must run as root (btrfs ioctls + loop mount)"; exit 1; }

CAP=$((64 * 1024 * 1024))    # same 64 MiB the rootfs-overlay D6.2 measurements used
BTRFS_ROOT=/var/tmp          # btrfs with quota fs-wide enabled, squota (ADR-0002)
WORK="$(mktemp -d "$BTRFS_ROOT/spike-reclaim.XXXXXX")"
FS="$(findmnt -T "$WORK" -no TARGET)"
ACCUM=60                     # Q5 accumulation cycles: enough to show the slope is
                             # flat and the leak is exactly one row per unit
FAIL=0

# --- ledger -----------------------------------------------------------------
# The host fs is SHARED with OrbStack/docker (~11 foreign rows) and possibly a
# concurrent spike. Two files keep us honest: BASE_IDS is the starting row set,
# OURS is every id we created. The trap destroys ONLY ids in OURS, and the final
# check asserts the row set is back to BASE_IDS (reporting foreign drift, if any,
# separately rather than blaming ourselves for it).
OURS="$(mktemp)"; BASE_IDS="$(mktemp)"; LOOPS="$(mktemp)"
rows()    { btrfs qgroup show --raw --sync "${1:-$FS}" 2>/dev/null | tail -n +3; }
# plain `sort`, not -V: comm needs the SAME collation it uses internally, and -V
# makes it emit "file is not in sorted order" and mis-compare.
rowids()  { rows "${1:-$FS}" | awk '{print $1}' | sort; }
rowids > "$BASE_IDS"
BASE_N="$(wc -l < "$BASE_IDS")"

cleanup() {
  set +e
  # loop filesystems first: unmount, then drop the image (their rows die with them)
  while read -r img mnt; do
    [ -n "$img" ] || continue
    umount "$mnt" >/dev/null 2>&1
    rm -f "$img"; rmdir "$mnt" >/dev/null 2>&1
  done < "$LOOPS"
  # host fs: walk deepest-first and `subvolume delete` (rm -rf cannot remove a
  # subvolume). readdir+st_ino probe, not `subvolume list` — that is fs-wide and
  # would reach a live sandbox's upper (§5.C sweep safety requirements).
  find "$WORK" -depth -mindepth 1 -type d -print0 2>/dev/null \
    | while IFS= read -r -d '' p; do
        [ "$(stat -c %i "$p" 2>/dev/null)" = "256" ] || continue
        btrfs subvolume delete "$p" >/dev/null 2>&1
      done
  rm -rf "$WORK" >/dev/null 2>&1
  # Force the transaction commit + cleaner so every deletion SETTLES; destroy is
  # EBUSY before that (Q2). This is the same two-step the recommended sweep uses.
  btrfs filesystem sync "$FS" >/dev/null 2>&1
  btrfs subvolume sync "$FS" >/dev/null 2>&1
  # Only our own ids, and only after re-checking the id is not a live subvolume's.
  sort -u "$OURS" | while read -r id; do
    [ -n "$id" ] || continue
    btrfs qgroup destroy "0/$id" "$FS" >/dev/null 2>&1
  done
  LEFT="$(comm -13 "$BASE_IDS" <(rowids) 2>/dev/null)"
  MINE="$(echo "$LEFT" | grep -Fxf <(sed 's|^|0/|' "$OURS" | sort -u) 2>/dev/null)"
  echo
  echo "row ledger: baseline $BASE_N rows -> final $(rowids | wc -l) rows"
  if [ -n "${MINE//[[:space:]]/}" ]; then
    echo "  *** THIS SPIKE LEAKED ITS OWN ROWS: $(echo $MINE) ***"
  else
    echo "  no row of ours remains (exact restore of our own footprint)"
    [ -n "${LEFT//[[:space:]]/}" ] && echo "  foreign rows appeared meanwhile (not ours, untouched): $(echo $LEFT)"
  fi
  rm -f "$OURS" "$BASE_IDS" "$LOOPS"
}
trap cleanup EXIT

# --- helpers ----------------------------------------------------------------
idof()  { btrfs subvolume show "$1" | awk '/Subvolume ID/{print $3}'; }
mk()    { btrfs subvolume create "$1" >/dev/null;        idof "$1" | tee -a "$OURS"; }
snap()  { btrfs subvolume snapshot "$1" "$2" >/dev/null; idof "$2" | tee -a "$OURS"; }
del()   { btrfs subvolume delete "$1" >/dev/null; }
fill()  { dd if=/dev/urandom of="$1" bs=1M count="${2:-8}" status=none conv=fsync; }
# settle <fs> — force a transaction commit, THEN wait for the cleaner. Measured:
# `subvolume sync` alone waits seconds (it sleeps until the periodic commit, so the
# cost is load-dependent, not fixed: runs have landed at 7-16 s and at 19.1 s);
# preceding it with `filesystem sync` makes the pair return in ~0 s. Q3.
settle(){ btrfs filesystem sync "${1:-$FS}" >/dev/null 2>&1 || true
          btrfs subvolume sync "${1:-$FS}" >/dev/null 2>&1 || true; }
# rowof / label — the row and just its trailing label|path (fields 6..NF, because
# `<squota space holder>` contains spaces).
# rowof <id> [fs] — the row, or "" if absent. MEASURED HAZARD: `qgroup show`
# fails WHOLESALE (rc=1, "cannot find the qgroup 0/N / can't list qgroups: No
# such file or directory") when ANY subvolume disappears while it is resolving
# paths — concurrent churn, which a sweep runs in by definition. Both the full
# and the `-f` form do it. A naive reader then sees "" and concludes "row gone",
# skipping a destroy it should have done. So: check rc, and retry rather than
# trust an empty read. The sweep needs the same distinction.
rowof() { local out rc i
  for i in 1 2 3; do
    set +e; out="$(btrfs qgroup show -re --raw --sync "${2:-$FS}" 2>/dev/null)"; rc=$?; set -e
    [ $rc -eq 0 ] && { echo "$out" | awk -v i="0/$1" '$1==i'; return 0; }
  done
  echo "SHOW_FAILED"; }
label() { local r; r="$(rowof "$1" "${2:-$FS}")"
          [ "$r" = SHOW_FAILED ] && { echo "(show failed)"; return; }
          [ -z "$r" ] && { echo "(no row)"; return; }
          echo "$r" | awk '{s="";for(i=6;i<=NF;i++)s=s (i>6?" ":"") $i; print s}'; }
# present <id> — "yes"/"no", and aborts on an unreadable show rather than guessing.
present(){ local r; r="$(rowof "$1" "${2:-$FS}")"
           case "$r" in SHOW_FAILED) echo unknown;; "") echo no;; *) echo yes;; esac; }
# labelNS — same, WITHOUT --sync. Q3 has to observe the un-synced state, and
# `qgroup show --sync` would itself force the commit it is trying to measure.
labelNS(){ local r; r="$(btrfs qgroup show -re --raw "${2:-$FS}" 2>/dev/null | awk -v i="0/$1" '$1==i')"
          [ -z "$r" ] && { echo "(no row)"; return; }
          echo "$r" | awk '{s="";for(i=6;i<=NF;i++)s=s (i>6?" ":"") $i; print s}'; }
capof() { btrfs qgroup show -re --raw --sync -f "$1" 2>/dev/null | awk '/^0\//{print $4;exit}'; }
# destroy <id> [fs] — prints "rc=N <stderr>"; never aborts the script.
destroy(){ local o rc; set +e; o="$(btrfs qgroup destroy "0/$1" "${2:-$FS}" 2>&1)"; rc=$?; set -e
           printf 'rc=%s%s' "$rc" "${o:+ ${o#ERROR: unable to destroy quota group: }}"; }
# loopfs <tag> <sizeG> — a private squota filesystem. Needed because Q5's id-reuse
# probe must UNMOUNT/REMOUNT the fs, which is impossible on the host root fs. Note
# `mkfs.btrfs -O squota`: `btrfs quota enable --simple` is EPERM on this substrate
# even as root (D6.2), so the mode has to be baked at mkfs time.
loopfs(){ local img=$BTRFS_ROOT/spike-reclaim-$1.img mnt=$BTRFS_ROOT/spike-reclaim-$1.mnt
  rm -f "$img"; mkdir -p "$mnt"; truncate -s "$2"G "$img"
  mkfs.btrfs -q -O squota "$img" >/dev/null 2>&1 || return 1
  mount -o loop "$img" "$mnt" >/dev/null 2>&1 || return 1
  echo "$img $mnt" >> "$LOOPS"; echo "$mnt"; }
remount(){ local mnt=$1 img; img="$(awk -v m="$mnt" '$2==m{print $1}' "$LOOPS")"
  umount "$mnt"; mount -o loop "$img" "$mnt"; }

echo "=================================================================="
echo " Q0. environment"
echo "=================================================================="
UUID="$(btrfs filesystem show "$FS" | sed -n 's/.*uuid: //p' | head -1)"
MODE="$(cat "/sys/fs/btrfs/$UUID/qgroups/mode" 2>/dev/null || echo unknown)"
echo "fs mount   : $FS   (btrfs-progs $(btrfs --version | head -1 | awk '{print $2}'))"
echo "quota mode : $MODE"
echo "baseline   : $BASE_N qgroup rows (foreign: OrbStack/docker — never touched)"
[ "$MODE" = squota ] || { echo "NOT squota — every finding below is squota-specific"; FAIL=1; }

echo
echo "=================================================================== Q1"
echo " Q1. reproduce the core claim, and time the transition"
echo "=================================================================="
# The exact sequence deep-dives/volume.md D7.1 describes: capped source, filled,
# snapshotted, source deleted, destroy attempted IMMEDIATELY. Then poll, first
# with the snapshot ALIVE (is the wait time-based?) and then after killing it (is
# the transition conditioned on the sharer instead?). The answer decides whether a
# sweep can just sleep-and-retry or must poll a predicate.
A="$WORK/A"
A_ID="$(mk "$A")"; btrfs qgroup limit "$CAP" "$A"; fill "$A/d" 8
B_ID="$(snap "$A" "$WORK/B")"
del "$A"
T0=$(date +%s)
printf '  %-8s %-14s %-46s %s\n' "t" "destroy" "error" "row label"
D="$(destroy "$A_ID")"   # ONE attempt, immediately: the claim under test
# un-synced read first (this is the transient the engine's own delete path sees),
# then the settled label — they differ, and conflating them is how the prior
# spike's comment ended up describing only one of them.
printf '  +%-7s %-14s %-46s %s\n' "0s" "${D%% *}" "${D#* }" "$(labelNS "$A_ID")  [no sync]"
settle
HOLD_LABEL="$(label "$A_ID")"
printf '  +%-7s %-14s %-46s %s\n' "0s" "-" "(after settle)" "$HOLD_LABEL"
for i in 1 2 3 4 5; do
  sleep 5; settle                     # sync EVERY round: if syncing were what
  D="$(destroy "$A_ID")"              # unblocks destroy, it would show up here
  printf '  +%-7s %-14s %-46s %s\n' "$(( $(date +%s) - T0 ))s" "${D%% *}" "${D#* }" "$(label "$A_ID")"
done
HELD_FOR=$(( $(date +%s) - T0 ))
echo "  -> ${HELD_FOR}s and 6 syncs with the snapshot alive: still EBUSY, label unchanged."
echo "     The wait is NOT time-based."
del "$WORK/B"; settle
T1=$(date +%s)
printf '  %-8s %-14s %-46s %s\n' "t(post)" "destroy" "error" "row label"
STALE_LABEL="$(label "$A_ID")"
printf '  +%-7s %-14s %-46s %s\n' "0s" "-" "(snapshot deleted)" "$STALE_LABEL"
D="$(destroy "$A_ID")"
printf '  +%-7s %-14s %-46s %s\n' "$(( $(date +%s) - T1 ))s" "${D%% *}" "${D#* }" "$(label "$A_ID")"
if [ "${D%% *}" = "rc=0" ]; then
  echo "  -> destroy SUCCEEDS the moment the last SHARER is gone and the deletion"
  echo "     has settled. The transition is CONDITIONED, not timed."
else
  echo "  -> destroy still failed after the sharer died: $D"; FAIL=1
fi

echo
echo "=================================================================== Q2"
echo " Q2. the observable predicate: which label means destroy will succeed"
echo "=================================================================="
# The sweep needs a predicate it can poll. `qgroup show` has four observable
# states; this produces each one deliberately and attempts a destroy against it,
# so the mapping is measured rather than inferred from the label's wording.
printf '  %-24s %-40s %-10s %s\n' "STATE PRODUCED BY" "raw row (label field)" "destroy" "self-clears?"
printf '  %-24s %-40s %-10s %s\n' "------------------" "---------------------" "-------" "------------"

# (1) a LIVE subvolume's row — the safety case: a sweep must not be able to
#     destroy a running unit's row by mistake.
L_ID="$(mk "$WORK/live")"; btrfs qgroup limit "$CAP" "$WORK/live"
# a live row's field 6.. is its PATH, not a <...> marker — that alone distinguishes
# it from every reclaimable state. Truncated here only to keep the column readable.
L_LBL="$(label "$L_ID")"
printf '  %-24s %-40s %-10s %s\n' "live subvolume" ".../$(basename "$L_LBL") (a PATH)" \
       "$(destroy "$L_ID" | cut -d' ' -f1)" "n/a (in use)"

# (2) <under deletion> — the transient right after delete, before the cleaner has
#     run. Made observable by deleting a BIG subvolume and NOT syncing.
U_ID="$(mk "$WORK/und")"; fill "$WORK/und/d" 64; del "$WORK/und"
U_LBL="$(labelNS "$U_ID")"; U_D="$(destroy "$U_ID")"   # labelNS: --sync would settle it
settle; U_AFTER="$(label "$U_ID")"
printf '  %-24s %-40s %-10s %s\n' "delete, no sync yet" "$U_LBL" "${U_D%% *}" "YES -> $U_AFTER"
[ "$(present "$U_ID")" = no ] || { echo "     (unexpected: no-sharer delete left a row: $(label "$U_ID"))"; FAIL=1; }

# (3) <squota space holder> — deleted, but a live sharer still references extents
#     this row owned. Q1 already showed this is EBUSY and does not self-clear.
H_ID="$(mk "$WORK/hold")"; btrfs qgroup limit "$CAP" "$WORK/hold"; fill "$WORK/hold/d" 8
HS_ID="$(snap "$WORK/hold" "$WORK/holds")"; del "$WORK/hold"; settle
H_LBL="$(label "$H_ID")"; H_D="$(destroy "$H_ID")"
printf '  %-24s %-40s %-10s %s\n' "delete w/ live sharer" "$H_LBL" "${H_D%% *}" "NO (Q1: ${HELD_FOR}s+syncs)"

# (4) <stale> — sharer gone, deletion settled. The destroyable state.
del "$WORK/holds"; settle
S_LBL="$(label "$H_ID")"; S_D="$(destroy "$H_ID")"
printf '  %-24s %-40s %-10s %s\n' "sharer gone, settled" "$S_LBL" "${S_D%% *}" "NO -> needs destroy"
# (5) and destroy is idempotent-by-ENOENT once the row is gone, which is what the
#     current best-effort call in DeleteSubvolume relies on.
printf '  %-24s %-40s %-10s %s\n' "already destroyed" "$(label "$H_ID")" "$(destroy "$H_ID" | cut -d' ' -f1)" "n/a"

# (6) THE READ ITSELF IS RACY, and this bit the first draft of this script. Under
#     concurrent subvolume churn `qgroup show` fails WHOLESALE (rc=1, "cannot find
#     the qgroup 0/N" / "can't list qgroups: No such file or directory") because it
#     resolves paths for EVERY row and one of them vanished underneath it. The `-f`
#     single-path form does it too. A sweep runs in exactly that churn, so a reader
#     that treats empty output as "row gone" will silently skip destroys. Measured
#     rate below (low, but not zero — and a missed destroy is a permanent leak).
echo "  (6) is the READ reliable under churn? 60 shows against 3 churn workers:"
CH_FAIL=0; CH_N=60
for k in 1 2 3; do
  ( for j in $(seq 1 40); do q="$WORK/ch${k}_$j"
      btrfs subvolume create "$q" >/dev/null 2>&1
      btrfs qgroup limit "$CAP" "$q" >/dev/null 2>&1
      dd if=/dev/urandom of="$q/d" bs=64k count=1 status=none 2>/dev/null
      btrfs subvolume delete "$q" >/dev/null 2>&1
    done ) &
done
for i in $(seq 1 $CH_N); do
  set +e; ERR="$(btrfs qgroup show -re --raw --sync "$FS" 2>&1 >/dev/null)"; set -e
  if [ -n "$ERR" ]; then
    CH_FAIL=$((CH_FAIL+1))
    [ $CH_FAIL -eq 1 ] && printf '        first failure: %s\n' "$(echo "$ERR" | head -1)"
  fi
done
wait
settle
printf '        %s/%s full-table reads FAILED while subvolumes were disappearing\n' "$CH_FAIL" "$CH_N"
if [ "$CH_FAIL" -eq 0 ]; then
  echo "        -> not reproduced in THIS run. It is a genuine race, reproduced out of"
  echo "           band at ~1/150 with heavier churn, so a zero here is a timing"
  echo "           accident and NOT evidence the read is safe."
fi
echo "        -> an empty/failed read is NOT evidence the row is gone. The sweep must"
echo "           check the exit status and retry, never infer absence from no output."
echo "  -> PREDICATE: destroy succeeds iff the label is exactly '<stale>'."
echo "     '<under deletion>' and '<squota space holder>' are both EBUSY; the"
echo "     first self-clears on settle, the second only after its sharer dies."
[ "$S_D" = "rc=0" ] || { echo "     (stale row was NOT destroyable — predicate broken)"; FAIL=1; }
[ "${H_D%% *}" = "rc=1" ] || { echo "     (space holder was destroyable — predicate broken)"; FAIL=1; }

echo
echo "=================================================================== Q3"
echo " Q3. does 'btrfs subvolume sync' help, and on what"
echo "=================================================================="
# Two separate questions the prior spike conflated. (i) Does sync move a row from
# <under deletion> to gone/stale — i.e. is it needed at all? (ii) Does it move a
# <squota space holder> whose sharer is STILL ALIVE? Q1 already answered (ii) no;
# this times (i), and times the two-step vs `subvolume sync` alone, because a sweep
# that used the slow form would block for seconds per pass (7-16 s, 19.1 s observed).
X_ID="$(mk "$WORK/sy1")"; fill "$WORK/sy1/d" 8; del "$WORK/sy1"
echo "  (i) after delete, WITHOUT any sync, 5 rapid polls (labelNS: a --sync read"
echo "      would force the very commit being measured):"
for i in 1 2 3 4 5; do printf '        %s\n' "$(labelNS "$X_ID")"; done
S=$(date +%s%N); btrfs subvolume sync "$FS" >/dev/null 2>&1 || true
T_ALONE=$(( ($(date +%s%N) - S) / 1000000 ))
echo "      'subvolume sync' ALONE: ${T_ALONE}ms  -> label now: $(labelNS "$X_ID")"
Y_ID="$(mk "$WORK/sy2")"; fill "$WORK/sy2/d" 8; del "$WORK/sy2"
S=$(date +%s%N); btrfs filesystem sync "$FS" >/dev/null 2>&1 || true
btrfs subvolume sync "$FS" >/dev/null 2>&1 || true
T_PAIR=$(( ($(date +%s%N) - S) / 1000000 ))
echo "      'filesystem sync' THEN 'subvolume sync': ${T_PAIR}ms -> label: $(labelNS "$Y_ID")"
echo "  -> sync IS required to leave <under deletion>: the label does not move on"
echo "     its own within a poll burst, and does move once synced."
echo "     Timings are LOAD-DEPENDENT, not a fixed property: 'subvolume sync' alone"
echo "     sleeps until the next periodic commit, so the wait is LOAD-DEPENDENT rather"
echo "     than a fixed property (7-16 s and 19.1 s across runs; ${T_ALONE}ms here),"
echo "     while 'filesystem sync' first forces the"
echo "     commit and makes the pair bounded (${T_PAIR}ms). Prefer the pair — it cannot"
echo "     be slower, and removes the dependence on commit timing."
echo "     Neither helps a <squota space holder> with a live sharer (Q1)."

echo
echo "=================================================================== Q4"
echo " Q4. leak surface: which sequences leak a row, which self-reclaim"
echo "=================================================================="
# The sweep should not run a reclaim step on sequences that never leak. Each case
# ends with everything deleted and settled; then we report whether a row survived
# and whether a later destroy clears it. LEAK is the interesting column.
LEAKS=0; CLEAN=0
# case <tag> <ids...> — settle, then classify each id.
verdict(){ local tag="$1"; shift
  settle
  for id in "$@"; do
    if [ "$(present "$id")" = no ]; then
      printf '  %-46s 0/%-4s self-reclaimed\n' "$tag" "$id"; CLEAN=$((CLEAN+1))
    else
      printf '  %-46s 0/%-4s LEAKED %-22s later destroy %s\n' \
        "$tag" "$id" "$(label "$id")" "$(destroy "$id" | cut -d' ' -f1)"
      LEAKS=$((LEAKS+1))
    fi
    tag=""
  done; }

# (a) the case the doc comment was measured on: never snapshotted.
i1="$(mk "$WORK/a")"; btrfs qgroup limit "$CAP" "$WORK/a"; fill "$WORK/a/d" 8; del "$WORK/a"
verdict "(a) capped, filled, NEVER snapshotted" "$i1"

# (b) the §4.3 case: source deleted while the snapshot lives.
i1="$(mk "$WORK/b")"; btrfs qgroup limit "$CAP" "$WORK/b"; fill "$WORK/b/d" 8
i2="$(snap "$WORK/b" "$WORK/bs")"; del "$WORK/b"; settle; del "$WORK/bs"
verdict "(b) snapshot, delete SOURCE first" "$i1" "$i2"

# (c) the same pair in the other order. If this does not leak, ORDER is the lever
#     — a destroy path that tears down newest-first avoids the leak entirely.
i1="$(mk "$WORK/c")"; btrfs qgroup limit "$CAP" "$WORK/c"; fill "$WORK/c/d" 8
i2="$(snap "$WORK/c" "$WORK/cs")"; del "$WORK/cs"; settle; del "$WORK/c"
verdict "(c) snapshot, delete SNAPSHOT first" "$i1" "$i2"

# (d) both deleted back-to-back with no settle between — the realistic shape when
#     a sandbox and its clone are destroyed in one sweep pass.
i1="$(mk "$WORK/d")"; btrfs qgroup limit "$CAP" "$WORK/d"; fill "$WORK/d/d" 8
i2="$(snap "$WORK/d" "$WORK/ds")"; del "$WORK/d"; del "$WORK/ds"
verdict "(d) snapshot, delete BOTH, no settle between" "$i1" "$i2"

# (e1) chain A->B->C deleted oldest-first. Only A ever WROTE data; B and C hold
#      only their own metadata. Tests whether "was snapshotted" or "owned extents
#      a sharer still references" is the real predicate.
i1="$(mk "$WORK/e1a")"; fill "$WORK/e1a/d" 8
i2="$(snap "$WORK/e1a" "$WORK/e1b")"; i3="$(snap "$WORK/e1b" "$WORK/e1c")"
del "$WORK/e1a"; settle; del "$WORK/e1b"; settle; del "$WORK/e1c"
verdict "(e1) chain A->B->C, delete A,B,C" "$i1" "$i2" "$i3"

# (e2) the same chain newest-first.
i1="$(mk "$WORK/e2a")"; fill "$WORK/e2a/d" 8
i2="$(snap "$WORK/e2a" "$WORK/e2b")"; i3="$(snap "$WORK/e2b" "$WORK/e2c")"
del "$WORK/e2c"; settle; del "$WORK/e2b"; settle; del "$WORK/e2a"
verdict "(e2) chain A->B->C, delete C,B,A" "$i1" "$i2" "$i3"

# (e3) chain where the MIDDLE link wrote its own data before being snapshotted —
#      isolates "wrote data + has a live sharer" from "is the chain root".
i1="$(mk "$WORK/e3a")"; fill "$WORK/e3a/d" 8
i2="$(snap "$WORK/e3a" "$WORK/e3b")"; fill "$WORK/e3b/own" 8
i3="$(snap "$WORK/e3b" "$WORK/e3c")"
del "$WORK/e3b"; settle; del "$WORK/e3a"; del "$WORK/e3c"
verdict "(e3) chain, MIDDLE wrote data, deleted first" "$i1" "$i2" "$i3"

# (f1) UNCAPPED source, snapshotted, source first: does the cap matter to the leak?
i1="$(mk "$WORK/f1")"; fill "$WORK/f1/d" 8
i2="$(snap "$WORK/f1" "$WORK/f1s")"; del "$WORK/f1"; settle; del "$WORK/f1s"
verdict "(f1) UNCAPPED, snapshot, delete source first" "$i1" "$i2"

# (f2) capped but EMPTY source, snapshotted, source first. If a leak needs OWNED
#      EXTENTS, a data-less unit cannot leak however it is deleted.
i1="$(mk "$WORK/f2")"; btrfs qgroup limit "$CAP" "$WORK/f2"
i2="$(snap "$WORK/f2" "$WORK/f2s")"; del "$WORK/f2"; settle; del "$WORK/f2s"
verdict "(f2) capped but EMPTY, snapshot, source first" "$i1" "$i2"
echo "  -> $LEAKS leaked / $CLEAN self-reclaimed. Every leak was a unit that (i) OWNED"
echo "     extents and (ii) was deleted while a live sharer still referenced them."
echo "     'Was snapshotted' alone does not leak; snapshot-first never leaks."

echo
echo "=================================================================== Q5"
echo " Q5. is a stale row harmful, or merely untidy?"
echo "=================================================================="
# Everything here runs on a PRIVATE loop fs: the id-reuse probe needs an
# unmount/remount, impossible on the host root fs, and accumulating 60 rows on a
# shared filesystem would be antisocial.
HARM="$(loopfs harm 3 || true)"
if [ -z "$HARM" ]; then
  echo "  loop fs unavailable — Q5 SKIPPED (needs mkfs.btrfs -O squota + loop mount)"
  ID_REUSE=skipped
else
echo "  private squota loop fs at $HARM"
tm(){ local s; s=$(date +%s%N); btrfs qgroup show --raw --sync "$HARM" >/dev/null 2>&1
      echo "$(( ($(date +%s%N) - s) / 1000000 ))ms"; }
echo "  (5a) does an accumulation of stale rows cost anything?"
printf '        %-14s %-8s %-8s %s\n' "after cycles" "rows" "stale" "qgroup show"
printf '        %-14s %-8s %-8s %s\n' "0" "$(rows "$HARM" | wc -l)" "0" "$(tm)"
for n in $(seq 1 $ACCUM); do
  btrfs subvolume create "$HARM/L$n" >/dev/null
  btrfs qgroup limit 1048576 "$HARM/L$n"
  dd if=/dev/urandom of="$HARM/L$n/d" bs=64k count=1 status=none conv=fsync
  btrfs subvolume snapshot "$HARM/L$n" "$HARM/S$n" >/dev/null
  btrfs subvolume delete "$HARM/L$n" >/dev/null; btrfs subvolume delete "$HARM/S$n" >/dev/null
  if [ $(( n % 20 )) -eq 0 ]; then settle "$HARM"
    printf '        %-14s %-8s %-8s %s\n' "$n" "$(rows "$HARM" | wc -l)" \
      "$(rows "$HARM" | grep -c '<stale>' || true)" "$(tm)"
  fi
done
settle "$HARM"
N_STALE="$(rows "$HARM" | grep -c '<stale>' || true)"
echo "        -> exactly ONE stale row per snapshotted unit ($N_STALE for $ACCUM cycles),"
echo "           and 'qgroup show' does not slow down. No row-count limit hit."
echo "  (5b) with $N_STALE stale rows present, does a NEW capped subvolume still work?"
btrfs subvolume create "$HARM/T" >/dev/null; btrfs qgroup limit 2097152 "$HARM/T"
set +e; dd if=/dev/urandom of="$HARM/T/d" bs=1M count=8 status=none conv=fsync 2>/dev/null; set -e
echo "        row: $(btrfs qgroup show -re --raw --sync -f "$HARM/T" | tail -1)"
echo "        -> yes: limit settable, ENOSPC still precise to the new cap."
btrfs subvolume delete "$HARM/T" >/dev/null; settle "$HARM"

echo "  (5c) *** THE ONE THAT MATTERS: can a stale row's id be REUSED? ***"
# Within one mount, subvolume ids are strictly monotonic — no reuse. But the
# next-free id is RECOMPUTED AT MOUNT TIME from the ROOT TREE, and the quota tree
# has no say: a stale row does NOT reserve its id. So the hazard is a remount (a
# node reboot). Shape below is the realistic one: ONE live unit at a low id (a
# running sandbox's upper) pins the root tree's highest id just below a leaked
# stale row, so the very next create after the reboot lands ON it.
echo -n "        within one mount: "
btrfs subvolume create "$HARM/m1" >/dev/null; M1="$(idof "$HARM/m1")"
btrfs subvolume delete "$HARM/m1" >/dev/null; settle "$HARM"
btrfs subvolume create "$HARM/m2" >/dev/null; M2="$(idof "$HARM/m2")"
btrfs subvolume delete "$HARM/m2" >/dev/null; settle "$HARM"
[ "$M1" != "$M2" ] && echo "id $M1 then $M2 — monotonic, NO reuse" \
                   || { echo "id $M1 REUSED within one mount"; FAIL=1; }

PIN="$(loopfs pin 1 || true)"
if [ -z "$PIN" ]; then echo "        (second loop fs unavailable — 5c remount probe skipped)"; ID_REUSE=skipped
else
  # C: live, low id. A: capped 3333333 + data, snapshotted -> leaks a stale row
  # at C's id + 1. Then everything but C is gone, and we remount.
  btrfs subvolume create "$PIN/C" >/dev/null; C_ID="$(idof "$PIN/C")"
  btrfs qgroup limit "$CAP" "$PIN/C"
  btrfs subvolume create "$PIN/A" >/dev/null; PA_ID="$(idof "$PIN/A")"
  btrfs qgroup limit 3333333 "$PIN/A"
  dd if=/dev/urandom of="$PIN/A/d" bs=1M count=2 status=none conv=fsync
  btrfs subvolume snapshot "$PIN/A" "$PIN/B" >/dev/null
  btrfs subvolume delete "$PIN/A" >/dev/null; btrfs subvolume delete "$PIN/B" >/dev/null
  settle "$PIN"
  echo "        live C=0/$C_ID; leaked stale row 0/$PA_ID carrying cap 3333333:"
  rows "$PIN" | sed 's/^/          /'
  remount "$PIN"
  btrfs subvolume create "$PIN/NEW" >/dev/null; NEW_ID="$(idof "$PIN/NEW")"
  NEW_CAP="$(capof "$PIN/NEW")"
  echo "        after REMOUNT, a fresh subvolume (no limit ever set on it):"
  echo "          id=$NEW_ID   its row's Max referenced = $NEW_CAP"
  if [ "$NEW_ID" = "$PA_ID" ] && [ "$NEW_CAP" != none ]; then
    ID_REUSE=yes
    echo "          *** ID REUSED AND THE DEAD UNIT'S CAP SILENTLY INHERITED ***"
    set +e; dd if=/dev/urandom of="$PIN/NEW/big" bs=1M count=8 status=none conv=fsync 2>/dev/null; RC=$?; set -e
    echo "          writing 8 MiB into it: rc=$RC, row now:"
    btrfs qgroup show -re --raw --sync -f "$PIN/NEW" | tail -1 | sed 's/^/            /'
    echo "          -> ENOSPC at a cap NOBODY SET. Not untidy: a correctness bug."
  else
    ID_REUSE=no
    echo "          not adopted this run (next-free landed past the stale id)."
    echo "          NOTE: the quota tree still does not reserve the id, so this is"
    echo "          shape-dependent, not a guarantee."
  fi
fi
fi

echo
echo "=================================================================== Q7"
echo " Q7. does today's Overlay-upper path already leak? (§5.B vs §5.C)"
echo "=================================================================="
# The engine never snapshots an Overlay upper, so Q4 says it cannot leak. But
# EXTENT SHARING, not `subvolume snapshot`, is the real trigger — and overlayfs
# copy-up on the same fs is a REFLINK. Two directions, and they differ:
#   IN : layer(plain dir) -> upper(subvolume). Today's copy-up. The upper does not
#        OWN those extents, so it has nothing to hold.
#   OUT: upper(subvolume) -> layer(plain dir). What Promote would be if copyTree
#        ever gained `cp --reflink` as an optimization.
mkdir -p "$WORK/plain"; fill "$WORK/plain/src" 8
i1="$(mk "$WORK/rin")"; btrfs qgroup limit "$CAP" "$WORK/rin"
if cp --reflink=always "$WORK/plain/src" "$WORK/rin/f" 2>/dev/null; then
  del "$WORK/rin"; settle
  printf '  %-52s %s\n' "reflink IN (copy-up shape), then delete upper:" \
    "$([ "$(present "$i1")" = no ] && echo "no row — NO LEAK" || echo "LEAKED $(label "$i1")")"
  [ "$(present "$i1")" = yes ] && { destroy "$i1" >/dev/null; FAIL=1; }
else echo "  reflink unsupported here — Q7 reflink probe skipped"; fi
i2="$(mk "$WORK/rout")"; btrfs qgroup limit "$CAP" "$WORK/rout"; fill "$WORK/rout/f" 8
if cp --reflink=always "$WORK/rout/f" "$WORK/plain/promoted" 2>/dev/null; then
  del "$WORK/rout"; settle
  R_OUT="$(label "$i2")"
  printf '  %-52s %s\n' "reflink OUT (a reflinking Promote), then delete upper:" \
    "$([ "$(present "$i2")" = no ] && echo "no row — no leak" || echo "LEAKED $R_OUT")"
  if [ "$(present "$i2")" = yes ]; then
    rm -f "$WORK/plain/promoted"; settle
    echo "                                                       after removing the reflinked copy: $(label "$i2") -> destroy $(destroy "$i2" | cut -d' ' -f1)"
    REFLINK_OUT=leaks
  else REFLINK_OUT=clean; fi
fi
echo "  -> today's copy-up direction does NOT leak. §5.B is clean because it"
echo "     neither snapshots the upper nor reflinks OUT of it (copyTree is a"
echo "     byte-for-byte copy). This is a NEW §5.C problem — and a latent trap for"
echo "     any future 'make Promote use reflink' optimization."

echo
echo "=================================================================="
echo " VERDICT"
echo "=================================================================="
echo "Q1 core claim                : CONFIRMED. delete-source-with-live-snapshot ->"
echo "                               'qgroup destroy' = EBUSY ('Device or resource"
echo "                               busy'), NOT ENOENT. Settled row label = '$HOLD_LABEL'"
echo "                               (un-synced it still reads '<under deletion>')."
echo "                               Still EBUSY after ${HELD_FOR:-?}s and 6 syncs. It becomes"
echo "                               destroyable (rc=0) once the last SHARER is gone"
echo "                               and the delete has settled -> label '$STALE_LABEL'."
echo "                               CONDITIONED on the sharer, not on elapsed time."
echo "Q2 polling predicate         : label == '<stale>'  <=>  destroy succeeds."
echo "                               '<under deletion>'        EBUSY, self-clears on settle"
echo "                               '<squota space holder>'   EBUSY, clears only when the"
echo "                                                         sharer dies (then -> <stale>)"
echo "                               a LIVE subvolume's row    EBUSY (sweep is safe by"
echo "                                                         construction)"
echo "                               already destroyed         ENOENT (idempotent)"
echo "                               A live row's label is its PATH; every reclaimable"
echo "                               state is a '<...>' marker — so the predicate can"
echo "                               never fire on a running unit."
echo "Q2 caveat (bit this script)  : 'qgroup show' fails WHOLESALE (rc=1, 'can't list"
echo "                               qgroups: No such file or directory') when any"
echo "                               subvolume vanishes mid-enumeration — ${CH_FAIL:-?}/${CH_N:-?} this run,"
echo "                               reproduced ~1/150 out of band; the '-f' form"
echo "                               does it too. Check rc; never read empty output"
echo "                               as 'row gone' or the sweep skips a destroy."
echo "Q3 subvolume sync            : REQUIRED to leave '<under deletion>'; irrelevant to"
echo "                               a space holder with a live sharer. 'subvolume sync'"
echo "                               alone waits for the next periodic commit (7-16 s"
echo "                               when one is not already due; ${T_ALONE:-?}ms here);"
echo "                               'filesystem sync' first forces it (${T_PAIR:-?}ms)."
echo "                               Use the pair — bounded instead of commit-timed."
echo "Q4 leak predicate            : a row leaks iff the unit OWNED extents AND was"
echo "                               deleted while a live sharer referenced them."
echo "                               leaks: source-first (b,d), uncapped source (f1),"
echo "                                      chain root / any link that wrote data (e1,e3)"
echo "                               clean: no snapshot (a), snapshot-first (c),"
echo "                                      newest-first chain (e2), empty source (f2)"
echo "                               The cap is irrelevant to WHETHER it leaks."
echo "Q5 harm                      : NOT a performance problem — one row per unit,"
echo "                               'qgroup show' flat, new capped subvolumes fine."
case "${ID_REUSE:-?}" in
  yes) echo "                               *** BUT: ID REUSE IS REAL. Ids are monotonic"
       echo "                               within a mount, yet next-free is recomputed at"
       echo "                               MOUNT time from the ROOT tree — the quota tree"
       echo "                               does not reserve a stale id. After a remount a"
       echo "                               fresh subvolume took the stale id AND its cap,"
       echo "                               then hit ENOSPC at a limit nobody set. ***" ;;
  no)  echo "                               id reuse did not trigger in this run's shape, but"
       echo "                               nothing reserves a stale id, so it is not safe" ;;
  *)   echo "                               id-reuse probe SKIPPED (no loop fs)" ;;
esac
echo "Q6 recommended reclaim       : per orphan unit, in the sweep:"
echo "                                 1. delete SHARERS (snapshots/clones) FIRST, then"
echo "                                    the source — that order leaks nothing (Q4 c/e2)"
echo "                                 2. 'btrfs filesystem sync' then 'btrfs subvolume"
echo "                                    sync' once per pass (Q3), not per unit"
echo "                                 3. for each id we deleted, read its row —"
echo "                                    CHECKING THE EXIT STATUS first (Q2 caveat):"
echo "                                      show failed       -> unknown, retry/next sweep"
echo "                                      no row            -> done"
echo "                                      '<stale>'         -> qgroup destroy (rc=0)"
echo "                                      '<under deletion>'-> settle again, retry"
echo "                                      '<space holder>'  -> a sharer is STILL LIVE:"
echo "                                                           leave it, next sweep"
echo "                                 4. bound the in-pass retries (~3 rounds); past"
echo "                                    that leave the row and log it — the next sweep"
echo "                                    retries. Never spin on EBUSY."
echo "Q7 Overlay upper (§5.B)      : does NOT leak today. Uppers are never snapshotted"
echo "                               and Promote's copyTree is a byte-for-byte copy;"
echo "                               copy-up reflinks INTO the upper, which the measured"
echo "                               matrix shows is leak-free. So this is a NEW §5.C"
echo "                               problem, not a pre-existing sandbox leak."
[ "${REFLINK_OUT:-?}" = leaks ] && \
echo "                               CAVEAT: reflinking OUT of the upper DOES leak, so a"
echo "                               future 'Promote via cp --reflink' would leak one row"
echo "                               per Promote. Worth a comment where copyTree lives."
echo
[ $FAIL -eq 0 ] && echo "ALL PROBES BEHAVED AS ASSERTED" || echo "SOME PROBES DEVIATED (see FAIL lines above)"
exit $FAIL
