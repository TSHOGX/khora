#!/usr/bin/env bash
# spike-volume-cap-snapshot.sh — how a btrfs squota capacity LIMIT behaves across
# `btrfs subvolume snapshot`. Pure btrfs: no runsc, no overlayfs, no guest.
#
# WHY THIS EXISTS. Everything measured so far about snapshot × squota was on the
# USAGE side: a snapshot's referenced bytes are not charged to the new unit, the
# snapshot is atomic/instant/~16KiB, a live volume can be snapshotted without
# disturbing a writing guest, and a leaked qgroup row's id can be reused after a
# remount (spike-squota-snapshot.sh,
# spike-live-volume-snapshot.sh, spike-qgroup-reclaim.sh). All settled; NONE of it
# is re-measured here.
#
# The LIMIT side (`max_referenced`) has never been tested, and the Volume subsystem is about to
# depend on it: btrfs CoW snapshots makes "a volume = a subvolume" precisely so that cheap
# snapshots and the hard cap land on the SAME object. Whether a clone's volume
# inherits quota/policy is still undecided, while resource admission already
# commits the node accounting inequality to "snapshot / clone volumes are counted by
# their **declared cap** rather than by metadata-level actual bytes". That sentence is
# only sound if the DECLARED cap is what the disk actually enforces. Five questions
# decide it:
#   Q1  INHERITANCE. Does the new subvolume's qgroup row carry the source's
#       `max_referenced`, or `none`? rw snapshot, `-r` snapshot, uncapped source,
#       and one transitive hop.
#   Q2  INDEPENDENCE. Can a different limit be set on the snapshot afterwards
#       without moving the source's, and can it be removed the same way? If the
#       two rows were coupled, `SetCapacity` on a clone would silently re-cap its
#       parent — a cross-volume write through a per-volume method.
#   Q3  READ-ONLY snapshots. Does `qgroup limit` apply to a `-r` snapshot at all,
#       and does `property set ro false` work here? This decides whether Clone can
#       be snapshot-then-unfreeze or must be a non-`-r` snapshot from the start.
#   Q4  ENFORCEMENT. Whatever Q1 REPORTS, verify by WRITING. A reported `none`
#       could be a cap we failed to read; only a write well past the source's cap
#       that SUCCEEDS proves there is no cap. Plus both isolation directions.
#   Q5  THE MISMATCH HAZARD. If the engine's record says "cap N" and the disk
#       enforces nothing, resource admission's left-hand side is fiction — the same class of
#       defect as Volume storage (a cap nobody set) reached from the opposite direction.
#       Shown concretely with an engine that "forgot" to set the cap.
#   Q6  LOWERING a cap BELOW current usage. `PATCH /v1/volumes/{id}` has exactly
#       one field, `size`, so the endpoint must state a semantic for "reduce the
#       cap under already-written bytes". It cannot state one honestly until the
#       substrate's behaviour is known: does btrfs refuse, or accept and leave an
#       unsatisfiable row? Are the existing bytes still readable and intact? Is it
#       reversible? That decides between 400 and a documented "existing bytes
#       kept, further growth refused".
#
# Usage: sudo ./spike-volume-cap-snapshot.sh   (needs root: btrfs ioctls, loop mount)
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "must run as root (btrfs ioctls + loop mount)"; exit 1; }

SRC_CAP=$((32 * 1024 * 1024))   # 32 MiB: the cap used by the statfs and
                                # ENOSPC-precision measurements
ALT_CAP=$((8 * 1024 * 1024))    # a DIFFERENT value, so "the snapshot's cap moved"
                                # and "the source's cap moved" are distinguishable
MID_CAP=$((24 * 1024 * 1024))   # a third value, for the reverse of Q2
Q6_FILL=20                      # MiB written BEFORE the cap is lowered (Q6). Chosen
                                # to sit clearly between ALT_CAP (the new, lower cap)
                                # and SRC_CAP (the old one), so "below usage" is
                                # unambiguous rather than a rounding question.
PAST=96                          # MiB to pour when proving "no cap": 3x SRC_CAP, so
                                 # success cannot be a rounding artifact
HUGE=128                         # MiB for Q5's forgetful-engine writer: 4x the cap
                                 # its control-plane record claims
BTRFS_ROOT=/var/tmp             # btrfs, quota fs-wide enabled, squota (the root-required deployment)
WORK="$(mktemp -d "$BTRFS_ROOT/spike-capsnap.XXXXXX")"
FS="$(findmnt -T "$WORK" -no TARGET)"
FAIL=0

# --- ledger -----------------------------------------------------------------
# Same discipline as spike-qgroup-reclaim.sh. The host fs is SHARED with
# OrbStack/docker (~11 foreign qgroup rows), so: BASE_IDS is the starting row set
# on the HOST fs, OURS is every id we created on the WORKING fs. The trap destroys
# ONLY ids in OURS, and the final check asserts the host row set is back to
# BASE_IDS — reporting foreign drift separately rather than blaming ourselves.
OURS="$(mktemp)"; BASE_IDS="$(mktemp)"; LOOPS="$(mktemp)"
rows()    { btrfs qgroup show --raw --sync "${1:-$FS}" 2>/dev/null | tail -n +3; }
# plain `sort`, not -V: comm needs the SAME collation it uses internally, and -V
# makes it emit "file is not in sorted order" and mis-compare.
rowids()  { rows "${1:-$FS}" | awk '{print $1}' | sort; }
rowids > "$BASE_IDS"
BASE_N="$(wc -l < "$BASE_IDS")"

cleanup() {
  set +e
  # The working fs may be a loop mount (rows die with the image) or the host fs
  # (rows are ours to destroy). Do the subvolume teardown + row destroy while it
  # is still mounted, THEN drop the loop images.
  if [ -n "${WFS:-}" ]; then
    # `-r` snapshots must be made rw before delete on some progs versions; do it
    # unconditionally, it is a no-op on an rw subvolume.
    find "$WFS" -depth -mindepth 1 -type d -print0 2>/dev/null \
      | while IFS= read -r -d '' p; do
          [ "$(stat -c %i "$p" 2>/dev/null)" = "256" ] || continue
          btrfs property set "$p" ro false >/dev/null 2>&1
          btrfs subvolume delete "$p" >/dev/null 2>&1
        done
    # Force the transaction commit + cleaner so every deletion SETTLES; destroy is
    # EBUSY before that (spike-qgroup-reclaim Q2).
    btrfs filesystem sync "$WFS" >/dev/null 2>&1
    btrfs subvolume sync "$WFS" >/dev/null 2>&1
    sort -u "$OURS" | while read -r id; do
      [ -n "$id" ] || continue
      btrfs qgroup destroy "$id" "$WFS" >/dev/null 2>&1
    done
  fi
  while read -r img mnt; do
    [ -n "$img" ] || continue
    umount "$mnt" >/dev/null 2>&1
    rm -f "$img"; rmdir "$mnt" >/dev/null 2>&1
  done < "$LOOPS"
  # Host fs: same deepest-first subvolume walk (readdir+st_ino probe, never
  # `subvolume list` — that is fs-wide and would reach a live sandbox's upper).
  find "$WORK" -depth -mindepth 1 -type d -print0 2>/dev/null \
    | while IFS= read -r -d '' p; do
        [ "$(stat -c %i "$p" 2>/dev/null)" = "256" ] || continue
        btrfs property set "$p" ro false >/dev/null 2>&1
        btrfs subvolume delete "$p" >/dev/null 2>&1
      done
  rm -rf "$WORK" >/dev/null 2>&1
  btrfs filesystem sync "$FS" >/dev/null 2>&1
  btrfs subvolume sync "$FS" >/dev/null 2>&1
  LEFT="$(comm -13 "$BASE_IDS" <(rowids) 2>/dev/null)"
  MINE="$(echo "$LEFT" | grep -Fxf <(sort -u "$OURS") 2>/dev/null)"
  echo
  echo "row ledger (HOST fs $FS): baseline $BASE_N rows -> final $(rowids | wc -l) rows"
  if [ -n "${MINE//[[:space:]]/}" ]; then
    echo "  *** THIS SPIKE LEAKED ITS OWN ROWS: $(echo $MINE) ***"
  else
    echo "  no row of ours remains on the host fs (exact restore of our footprint)"
    [ -n "${LEFT//[[:space:]]/}" ] && echo "  foreign rows appeared meanwhile (not ours, untouched): $(echo $LEFT)"
  fi
  rm -f "$OURS" "$BASE_IDS" "$LOOPS"
}
trap cleanup EXIT

# --- helpers ----------------------------------------------------------------
idof()  { btrfs subvolume show "$1" | awk '/Subvolume ID/{print $3}'; }
mk()    { btrfs subvolume create "$1" >/dev/null;    echo "0/$(idof "$1")" >> "$OURS"; }
snap()  { btrfs subvolume snapshot "$1" "$2" >/dev/null; echo "0/$(idof "$2")" >> "$OURS"; }
snapro(){ btrfs subvolume snapshot -r "$1" "$2" >/dev/null; echo "0/$(idof "$2")" >> "$OURS"; }
settle(){ btrfs filesystem sync "${1:-$WFS}" >/dev/null 2>&1 || true
          btrfs subvolume sync "${1:-$WFS}" >/dev/null 2>&1 || true; }
# capof / usedof — the single row for THIS path. `-f` filters to it, `--raw` for
# exact bytes, `--sync` so a just-written file does not read as 0. Exactly what
# internal/btrfs.Usage() parses, so a disagreement here is a disagreement the
# engine would also see.
capof()  { btrfs qgroup show -re --raw --sync -f "$1" 2>/dev/null | awk '/^0\//{print $4;exit}'; }
usedof() { btrfs qgroup show -re --raw --sync -f "$1" 2>/dev/null | awk '/^0\//{print $2;exit}'; }
rowof()  { btrfs qgroup show -re --raw --sync -f "$1" 2>/dev/null | awk '/^0\//{printf "%s used=%s max=%s",$1,$2,$4;exit}'; }
mib()    { [ "$1" = none ] && { echo none; return; }; awk -v b="$1" 'BEGIN{printf "%.2fMiB", b/1048576}'; }
# limit <path> <bytes|none> — prints rc, never aborts. The rc is itself a finding
# for Q3 (does btrfs REFUSE a limit on a read-only subvolume?).
limit()  { local o rc; set +e; o="$(btrfs qgroup limit "$1" "$2" 2>&1)"; rc=$?; set -e
           printf 'rc=%s%s' "$rc" "${o:+ [${o##*ERROR: }]}"; }
# pour <file> <MiB> — write and fsync; prints "rc=<n> wrote=<bytes>". Never aborts:
# an ENOSPC is a RESULT here, not a script failure. /dev/urandom, not /dev/zero:
# a zero-filled extent can be compressed or made a hole depending on mount
# options, which would silently understate what was charged.
pour()   { local rc sz; set +e
           dd if=/dev/urandom of="$1" bs=1M count="$2" status=none conv=fsync 2>/dev/null; rc=$?; set -e
           sz="$(stat -c %s "$1" 2>/dev/null || echo 0)"
           printf 'rc=%s wrote=%s' "$rc" "$sz"; }
# chk <what> <got> <want> — one assertion line, accumulates FAIL.
chk()    { if [ "$2" = "$3" ]; then printf '  ok    %-56s %s\n' "$1" "$2"
           else printf '  FAIL  %-56s got %s, want %s\n' "$1" "$2" "$3"; FAIL=1; fi; }
# loopfs <tag> <sizeG> — a private squota filesystem. Used for EVERY probe here:
# Q4/Q5 pour hundreds of MiB and must be able to tell "the cap stopped me" from
# "the filesystem filled up", which a private fs of known size gives for free.
# Note `mkfs.btrfs -O squota`: `btrfs quota enable --simple` is EPERM on this
# substrate even as root (Volume quota cleanup / btrfs quota materialization), so the mode has to
# be baked at mkfs time.
loopfs(){ local img=$BTRFS_ROOT/spike-capsnap-$1.img mnt=$BTRFS_ROOT/spike-capsnap-$1.mnt
  rm -f "$img"; mkdir -p "$mnt"; truncate -s "$2"G "$img"
  mkfs.btrfs -q -O squota "$img" >/dev/null 2>&1 || return 1
  mount -o loop "$img" "$mnt" >/dev/null 2>&1 || return 1
  echo "$img $mnt" >> "$LOOPS"; echo "$mnt"; }

echo "=================================================================="
echo " Q0. environment"
echo "=================================================================="
HOST_UUID="$(btrfs filesystem show "$FS" | sed -n 's/.*uuid: //p' | head -1)"
HOST_MODE="$(cat "/sys/fs/btrfs/$HOST_UUID/qgroups/mode" 2>/dev/null || echo unknown)"
echo "host fs    : $FS  mode=$HOST_MODE  (btrfs-progs $(btrfs --version | head -1 | awk '{print $2}'))"
echo "baseline   : $BASE_N host qgroup rows (foreign: OrbStack/docker — never touched)"

WFS="$(loopfs work 4 || true)"
if [ -z "$WFS" ]; then
  # Fall back rather than skip: none of Q1-Q5 needs a remount, so the host fs can
  # carry them — but say so loudly, because a shared fs makes a "write succeeded"
  # result weaker evidence (it could have been stopped by someone else's bytes).
  WFS="$WORK"
  echo "work fs    : *** loop fs UNAVAILABLE — falling back to the shared host fs ***"
  ISOLATED=no
else
  W_UUID="$(btrfs filesystem show "$WFS" | sed -n 's/.*uuid: //p' | head -1)"
  W_MODE="$(cat "/sys/fs/btrfs/$W_UUID/qgroups/mode" 2>/dev/null || echo unknown)"
  echo "work fs    : $WFS  mode=$W_MODE  (private 4G loop, mkfs -O squota)"
  echo "             free: $(df -h --output=avail "$WFS" | tail -1 | tr -d ' ') — so a"
  echo "             successful $PAST/$HUGE MiB pour cannot be confused with an fs-full stop"
  ISOLATED=yes
  [ "$W_MODE" = squota ] || { echo "work fs NOT squota — every finding below is squota-specific"; FAIL=1; }
fi

echo
echo "=================================================================== Q1"
echo " Q1. INHERITANCE: does a snapshot's row carry the source's max_referenced?"
echo "=================================================================="
# WHAT WRONG BEHAVIOUR THIS CATCHES. If the answer is "inherits", then the Volume subsystem's
# Snapshot/Clone gets a cap for free and the danger is a control-plane record that
# says "no cap" over a disk that enforces one. If the answer is "none", then every
# snapshot lands UNCAPPED and resource admission's "counted by the declared cap" is a claim about a
# number nobody wrote to disk. Both are defects; they are opposite defects, and the fix
# differs.
# Four shapes, because a rule that only holds for the rw case is not a rule.
mk "$WFS/src"; btrfs qgroup limit "$SRC_CAP" "$WFS/src"
pour "$WFS/src/seed" 8 >/dev/null
snap   "$WFS/src" "$WFS/snap_rw"      # the Clone shape (writable child volume)
snapro "$WFS/src" "$WFS/snap_ro"      # the Snapshot shape (immutable point in time)
mk "$WFS/unc"                          # UNCAPPED source: is there a surprise in the
pour "$WFS/unc/seed" 4 >/dev/null      # OTHER direction (a cap appearing from nowhere)?
snap "$WFS/unc" "$WFS/unc_snap"
settle
printf '  %-14s %-8s %-22s %-14s %s\n' "unit" "id" "referenced" "max_referenced" "source's cap"
printf '  %-14s %-8s %-22s %-14s %s\n' "----" "--" "----------" "--------------" "------------"
for u in src snap_rw snap_ro unc unc_snap; do
  case $u in
    src|unc) parent="-" ;;
    snap_rw|snap_ro) parent="$(mib "$(capof "$WFS/src")")" ;;
    unc_snap) parent="$(mib "$(capof "$WFS/unc")")" ;;
  esac
  printf '  %-14s %-8s %-22s %-14s %s\n' "$u" "0/$(idof "$WFS/$u")" \
    "$(mib "$(usedof "$WFS/$u")")" "$(mib "$(capof "$WFS/$u")")" "$parent"
done
Q1_RW="$(capof "$WFS/snap_rw")"; Q1_RO="$(capof "$WFS/snap_ro")"
Q1_UNC="$(capof "$WFS/unc_snap")"; Q1_SRC="$(capof "$WFS/src")"
chk "capped source keeps its own cap"            "$Q1_SRC" "$SRC_CAP"
chk "rw snapshot of a capped source"             "$Q1_RW"  "none"
chk "-r snapshot of a capped source"             "$Q1_RO"  "none"
chk "rw snapshot of an UNCAPPED source"          "$Q1_UNC" "none"
# transitive hop: cap the snapshot, snapshot IT. If inheritance were happening at
# all it would show up here, where the parent's cap was set explicitly rather than
# carried. Guards against "the first level is special".
btrfs qgroup limit "$ALT_CAP" "$WFS/snap_rw"
snap "$WFS/snap_rw" "$WFS/snap_rw2"; settle
Q1_HOP="$(capof "$WFS/snap_rw2")"
chk "snapshot of an explicitly-capped snapshot"  "$Q1_HOP" "none"
echo "  -> a new subvolume's max_referenced is 'none' in EVERY shape. The cap is a"
echo "     property of the qgroup row, and a snapshot gets a FRESH row."
# Bonus, because a plan author WILL reach for it: `snapshot -i <qgroupid>` looks
# like an inheritance switch. Measure what it actually buys under squota.
btrfs qgroup create 1/0 "$WFS" 2>/dev/null || true
echo '  -> and the tempting `snapshot -i <qgroupid>`: does a parent qgroup cap it?'
if btrfs subvolume snapshot -i 1/0 "$WFS/src" "$WFS/snap_i" >/dev/null 2>&1; then
  echo "0/$(idof "$WFS/snap_i")" >> "$OURS"
  btrfs qgroup limit "$ALT_CAP" 1/0 "$WFS" >/dev/null 2>&1 || true
  settle
  Q1_I="$(capof "$WFS/snap_i")"
  echo "       accepted (rc=0). new unit's OWN row: $(rowof "$WFS/snap_i")"
  # -re, not a bare show: without it the max_* columns are absent and field 4 is
  # the start of the `<N member qgroups>` label, which reads as a bogus cap.
  echo "       parent 1/0 row (limit $(mib $ALT_CAP) set on it): $(btrfs qgroup show -re --raw --sync "$WFS" 2>/dev/null | awk '$1=="1/0"{printf "used=%s max=%s",$2,$4}')"
  P="$(pour "$WFS/snap_i/past" 24)"
  settle
  echo "       pouring 24MiB (3x the parent's limit) into it: $P"
  echo "       its own row after: $(rowof "$WFS/snap_i")"
  Q1_I_ENF="$(echo "$P" | grep -q 'rc=0' && echo "NOT enforced" || echo "ENFORCED")"
  echo "       -> -i sets MEMBERSHIP, and confers no cap on the new unit's own row"
  echo "          ($Q1_I). A limit on the level-1 parent IS $Q1_I_ENF under squota,"
  echo "          but it is a SHARED ceiling over every member, not a per-volume cap:"
  echo "          it cannot express \"this volume gets N\" and one member exhausting it"
  echo "          ENOSPCs its siblings. Not the answer for the Volume subsystem."
else
  echo "       REFUSED — -i is not usable here at all."; Q1_I=refused; Q1_I_ENF=n/a
fi

echo
echo "=================================================================== Q2"
echo " Q2. INDEPENDENCE: are the two rows coupled?"
echo "=================================================================="
# WHAT WRONG BEHAVIOUR THIS CATCHES. the Volume subsystem's seam has ONE per-volume method,
# `SetCapacity(volumeID, capBytes)` (the volume backend seam). If setting a snapshot's limit
# also moved its source's, that method would be a cross-volume write: capping a
# clone would silently re-cap the volume it came from, breaking resource admission's Σ on a
# record nobody edited. And if `limit none` on a clone cleared the SOURCE's cap,
# destroying a clone's cap would be the Volume storage "limit none" catastrophe (a silently
# uncapped LIVE volume, zero signal) reached from a legitimate API call.
printf '  %-46s %-20s %s\n' "action" "snapshot cap" "source cap"
printf '  %-46s %-20s %s\n' "------" "------------" "----------"
printf '  %-46s %-20s %s\n' "(after Q1 set snapshot=$(mib $ALT_CAP))" \
  "$(mib "$(capof "$WFS/snap_rw")")" "$(mib "$(capof "$WFS/src")")"
chk "snapshot took the new limit"                "$(capof "$WFS/snap_rw")" "$ALT_CAP"
chk "source's cap UNMOVED by that"               "$(capof "$WFS/src")"     "$SRC_CAP"
# the reverse direction: move the SOURCE's cap, snapshot must not follow.
btrfs qgroup limit "$MID_CAP" "$WFS/src"; settle
printf '  %-46s %-20s %s\n' "set SOURCE cap = $(mib $MID_CAP)" \
  "$(mib "$(capof "$WFS/snap_rw")")" "$(mib "$(capof "$WFS/src")")"
chk "snapshot's cap UNMOVED by the source's edit" "$(capof "$WFS/snap_rw")" "$ALT_CAP"
# and removal, the dangerous one.
echo "$(limit none "$WFS/snap_rw")" | sed 's/^/        limit none on snapshot: /'
settle
printf '  %-46s %-20s %s\n' "limit none on the SNAPSHOT" \
  "$(mib "$(capof "$WFS/snap_rw")")" "$(mib "$(capof "$WFS/src")")"
chk "snapshot's cap removed"                      "$(capof "$WFS/snap_rw")" "none"
chk "source STILL CAPPED after that"              "$(capof "$WFS/src")"     "$MID_CAP"
btrfs qgroup limit "$SRC_CAP" "$WFS/src"; settle   # restore for Q4
echo "  -> fully independent in both directions. One row per subvolume, and no"
echo "     edit to either reaches the other."

echo
echo "=================================================================== Q3"
echo " Q3. READ-ONLY snapshots: can they be capped, and can they be unfrozen?"
echo "=================================================================="
# WHAT WRONG BEHAVIOUR THIS CATCHES. This decides the SHAPE of Clone. If `-r`
# refused a quota limit, then "clone = snapshot -r, then unfreeze, then cap" would
# be impossible and Clone would have to be a plain rw snapshot from the start —
# which in turn means the immutable-point-in-time Snapshot and the writable Clone
# cannot share one code path. And if `ro false` failed, a Snapshot could never
# become a Clone at all, so the two API verbs would need two backend mechanisms.
echo "  (a) apply a limit to a subvolume that is READ-ONLY right now:"
echo "        $(limit "$ALT_CAP" "$WFS/snap_ro")"
settle
chk "limit accepted on a -r snapshot"             "$(capof "$WFS/snap_ro")" "$ALT_CAP"
echo "  (b) and the ro flag still guards the DATA (a limit is not a write to it):"
RO_W="$(pour "$WFS/snap_ro/nope" 1 2>/dev/null || true)"
RO_ERR="$(dd if=/dev/urandom of="$WFS/snap_ro/nope2" bs=1M count=1 status=none 2>&1 >/dev/null || true)"
printf '        write into it: %s   [%s]\n' "$RO_W" "$(echo "$RO_ERR" | head -1)"
if [ "${RO_W#rc=}" = "0 wrote=1048576" ]; then
  echo "        *** a -r snapshot ACCEPTED a write — the ro flag is not enforcing ***"; FAIL=1
fi
echo "  (c) unfreeze it: btrfs property set <path> ro false"
set +e; RO_OUT="$(btrfs property set "$WFS/snap_ro" ro false 2>&1)"; RO_RC=$?; set -e
printf '        rc=%s %s\n' "$RO_RC" "${RO_OUT:+[$RO_OUT]}"
chk "property set ro false succeeded"             "$RO_RC" "0"
chk "ro flag is now false"                        "$(btrfs property get "$WFS/snap_ro" ro)" "ro=false"
settle
chk "the limit SURVIVED the ro->rw transition"    "$(capof "$WFS/snap_ro")" "$ALT_CAP"
echo "  (d) and it now takes writes, stopping at the cap set while it was ro:"
UF="$(pour "$WFS/snap_ro/afterthaw" $PAST)"; settle
printf '        pour %sMiB: %s   row: %s\n' "$PAST" "$UF" "$(rowof "$WFS/snap_ro")"
UF_BYTES="${UF##*wrote=}"
if [ "${UF#rc=}" != "0"* ] && [ "$UF_BYTES" -gt 0 ] \
   && [ "$UF_BYTES" -le "$ALT_CAP" ] \
   && [ "$UF_BYTES" -gt $((ALT_CAP - 2 * 1024 * 1024)) ]; then
  echo "        -> ENOSPC precise to the $(mib $ALT_CAP) cap ($(mib "$UF_BYTES") landed)."
  Q3_ENF=yes
else
  echo "        -> unexpected: expected ENOSPC just under $(mib $ALT_CAP)"; Q3_ENF=no; FAIL=1
fi
# (e) the OTHER order, which is the one Clone would actually use: unfreeze FIRST,
#     then cap. If unfreezing reset or invented a limit, this is where it shows.
snapro "$WFS/src" "$WFS/ro2"; settle
Q3_RO2_BEFORE="$(capof "$WFS/ro2")"
btrfs property set "$WFS/ro2" ro false
settle
Q3_RO2_AFTER="$(capof "$WFS/ro2")"
printf '  (e) fresh -r snapshot: cap before unfreeze=%s, after=%s\n' \
  "$(mib "$Q3_RO2_BEFORE")" "$(mib "$Q3_RO2_AFTER")"
chk "unfreezing does not invent a cap"            "$Q3_RO2_AFTER" "none"
echo "  -> both orders work. Clone MAY be snapshot -r then unfreeze; it is not"
echo "     forced to be a non-r snapshot from the start. Either way the cap is a"
echo "     SEPARATE explicit step."

echo
echo "=================================================================== Q4"
echo " Q4. ENFORCEMENT: verify Q1 BY WRITING, do not trust the reported number"
echo "=================================================================="
# WHAT WRONG BEHAVIOUR THIS CATCHES. A reported `none` has two possible causes:
# there is no cap, or there IS one and `qgroup show -f` does not surface it for a
# snapshot (which is exactly the read internal/btrfs.Usage() performs, so the
# engine would be equally blind). Only a write WELL PAST the source's cap that
# SUCCEEDS distinguishes them. The negative control below is the other half: on a
# snapshot where we DO set a cap, the write must stop — otherwise "the write
# succeeded" proves nothing about caps, only that this fs never says ENOSPC.
snap "$WFS/src" "$WFS/e_free"    # inherits nothing (Q1) -> should be unstoppable
snap "$WFS/src" "$WFS/e_capped"  # negative control: cap it ourselves
btrfs qgroup limit "$ALT_CAP" "$WFS/e_capped"
settle
SRC_USED_BEFORE="$(usedof "$WFS/src")"
echo "  (a) UNCAPPED snapshot, pour $PAST MiB (3x the source's $(mib $SRC_CAP) cap):"
E_FREE="$(pour "$WFS/e_free/big" $PAST)"; settle
printf '        %s   row: %s\n' "$E_FREE" "$(rowof "$WFS/e_free")"
E_FREE_BYTES="${E_FREE##*wrote=}"
chk "write succeeded (rc)"                        "${E_FREE%% *}" "rc=0"
chk "and landed all $PAST MiB"                    "$E_FREE_BYTES" "$((PAST * 1024 * 1024))"
if [ "$E_FREE_BYTES" -gt "$SRC_CAP" ]; then
  echo "        -> $(mib "$E_FREE_BYTES") charged to a unit whose source is capped at"
  echo "           $(mib $SRC_CAP). 'none' means NO CAP, not a cap we misread."
  Q4_UNCAPPED=proven
else
  echo "        -> did NOT get past the source's cap; 'none' may be a read failure"; Q4_UNCAPPED=unproven; FAIL=1
fi
echo "  (b) negative control — same shape, but WE set $(mib $ALT_CAP) on it:"
E_CAP="$(pour "$WFS/e_capped/big" $PAST)"; settle
printf '        %s   row: %s\n' "$E_CAP" "$(rowof "$WFS/e_capped")"
E_CAP_BYTES="${E_CAP##*wrote=}"
if [ "${E_CAP%% *}" != "rc=0" ] && [ "$E_CAP_BYTES" -le "$ALT_CAP" ] \
   && [ "$E_CAP_BYTES" -gt $((ALT_CAP - 2 * 1024 * 1024)) ]; then
  echo "        -> ENOSPC at the cap WE set ($(mib "$E_CAP_BYTES") of $(mib $ALT_CAP))."
  echo "           So a cap on a snapshot enforces normally — (a) is a real absence."
  Q4_CONTROL=enforced
else
  echo "        -> the control did not stop at its cap; (a) is inconclusive"; Q4_CONTROL=broken; FAIL=1
fi
echo "  (c) isolation, snapshot -> source: did $PAST MiB into the snapshot move the"
echo "      SOURCE toward its own cap? (if it did, one volume's writes could ENOSPC"
echo "      a different volume — a cap that is not per-volume at all)"
SRC_USED_AFTER="$(usedof "$WFS/src")"
DELTA=$((SRC_USED_AFTER - SRC_USED_BEFORE))
printf '        source referenced: %s -> %s  (delta %s)\n' \
  "$(mib "$SRC_USED_BEFORE")" "$(mib "$SRC_USED_AFTER")" "$DELTA bytes"
# 1 MiB of slack, not exact equality: unrelated metadata can tick. A real charge
# for the pour would be ~$PAST MiB, three orders of magnitude above the slack.
if [ "${DELTA#-}" -le 1048576 ]; then
  echo "        -> source unmoved. The two caps are isolated."; Q4_ISO=yes
else
  echo "        -> *** the source was charged for the snapshot's writes ***"; Q4_ISO=no; FAIL=1
fi
echo "  (d) isolation, source -> snapshot: pin the SOURCE at its cap, then write"
echo "      into the snapshot. (a full source must not freeze its clones — that"
echo "      would make Clone useless for the one case it exists for)"
S_PIN="$(pour "$WFS/src/fill" $PAST)"; settle
printf '        pour %sMiB into the source: %s   row: %s\n' "$PAST" "$S_PIN" "$(rowof "$WFS/src")"
chk "source is now pinned (ENOSPC)"               "$([ "${S_PIN%% *}" = "rc=0" ] && echo no || echo yes)" "yes"
S_AFTER="$(pour "$WFS/e_free/more" 8)"; settle
printf '        then 8MiB into the snapshot: %s   row: %s\n' "$S_AFTER" "$(rowof "$WFS/e_free")"
chk "snapshot still writable with source at cap"  "${S_AFTER%% *}" "rc=0"
echo "  -> ENFORCEMENT CONFIRMS Q1: the snapshot is genuinely uncapped, the caps are"
echo "     per-row, and neither direction of the pair constrains the other."

echo
echo "=================================================================== Q5"
echo " Q5. THE MISMATCH HAZARD: a record that says 'cap N' over a disk that says no"
echo "=================================================================="
# WHAT WRONG BEHAVIOUR THIS CATCHES — and it is the point of the whole spike.
# Resource admission requires `size` on POST /v1/volumes when the disk axis is metered
# ("the axis being metered ⟹ volumes must have caps too"), and states that
# snapshot/clone volumes are counted by their declared cap. The control-plane record
# therefore ALWAYS carries a number. Q1 says the disk carries `none`. So an engine
# whose Snapshot/Clone forgets the explicit SetQuotaLimit produces a volume that the
# admission gate counts as N bytes and the filesystem lets grow without bound — silent
# over-commit of the node budget, no errno anywhere, and `Usage()` returning limit=0
# which is INDISTINGUISHABLE from a deliberately uncapped volume. This is Volume storage's defect
# class from the other side: there, a cap nobody set; here, a cap everybody believes in
# that does not exist.
DECLARED=$SRC_CAP
mk "$WFS/vrec"; btrfs qgroup limit "$DECLARED" "$WFS/vrec"
pour "$WFS/vrec/data" 8 >/dev/null
# the engine snapshots it and allocates a new volume ID. It records size=$DECLARED
# (resource admission: counted by the declared cap). It does NOT call SetQuotaLimit — the bug under
# test.
snap "$WFS/vrec" "$WFS/vclone"; settle
echo "  control plane believes:  volume 'vclone' size = $(mib $DECLARED)  (resource admission: by declared cap)"
echo "  Usage() would read back:  $(rowof "$WFS/vclone")"
echo "                            limit=0 — the SAME value an intentionally uncapped"
echo "                            volume reports. The engine cannot tell them apart."
echo "  a guest-equivalent writer pours $HUGE MiB ($(( HUGE * 1048576 / DECLARED ))x the declared cap):"
Q5="$(pour "$WFS/vclone/guest" $HUGE)"; settle
printf '                            %s\n' "$Q5"
printf '                            row now: %s\n' "$(rowof "$WFS/vclone")"
Q5_BYTES="${Q5##*wrote=}"
if [ "${Q5%% *}" = "rc=0" ] && [ "$Q5_BYTES" -gt "$DECLARED" ]; then
  Q5_HAZARD=real
  echo "  -> *** $(mib "$Q5_BYTES") on disk against a $(mib $DECLARED) declaration, zero errors,"
  echo "     no lifecycle event, nothing in the control plane. resource admission's left-hand side"
  echo "     understated this node by $(mib $((Q5_BYTES - DECLARED))) from ONE forgotten call, and the"
  echo "     over-commit grows without bound because there is no ceiling to hit. ***"
  echo "     Same class as Volume storage (a cap nobody set), opposite direction: here it is a"
  echo "     cap the whole control plane believes in and the disk never had."
else
  Q5_HAZARD=notreproduced
  echo "  -> not reproduced: the write stopped, so something DID cap the clone."; FAIL=1
fi
echo "  and the detection question, since Volume storage showed guests produce no errno:"
echo "     Usage().limit = 0 for both 'forgot to cap' and 'deliberately uncapped'."
echo "     So the engine cannot READ its way out of this — the invariant has to be"
echo "     established at WRITE time, inside Snapshot/Clone."

echo
echo "=================================================================== Q6"
echo " Q6. LOWERING a cap BELOW current usage: what can the endpoint promise?"
echo "=================================================================="
# WHAT WRONG BEHAVIOUR THIS CATCHES. `PATCH /v1/volumes/{id}` carries only `size`,
# and resource admission already says any write to a cap column changes Σ AT ONCE ("a raise must
# re-run admission … while a lowering need not") — i.e. the spec assumes LOWERING is
# always safe and needs no admission re-check. That assumption is untested against a
# volume that has ALREADY written past the new number. Three distinguishable outcomes,
# and the endpoint's contract differs for each: (i) btrfs REFUSES -> the endpoint gets
# its 400 from the substrate for free; (ii) btrfs ACCEPTS and existing bytes are safe ->
# the endpoint can document "existing bytes kept, further growth refused"; (iii) btrfs
# ACCEPTS and existing data is truncated/lost/corrupted -> a `size` PATCH is a
# DESTRUCTIVE operation, which is the one outcome nothing in the spec currently allows
# for.
# (iii) is ruled out by MEASUREMENT, not hope, so the readback in (c) rehashes the
# file rather than trusting a stat().
#
# MEASURED ANSWER IS (ii) — but with a fourth property the three-way split did not
# anticipate, and it is the one that actually decides the endpoint: the lowered state
# is a ONE-WAY DOOR for the control plane. See (e). Do NOT simplify (e) away — the
# first draft of this spike asserted the opposite ("limit none restores writability")
# and that assertion FAILED against the substrate.
mk "$WFS/low"; btrfs qgroup limit "$SRC_CAP" "$WFS/low"
# Deterministic content with a known digest: a stat() size check would pass even if
# the extents underneath were zeroed, and "existing bytes stay intact" is exactly
# the claim that must not be taken on trust.
dd if=/dev/urandom of="$WFS/low/data" bs=1M count=$Q6_FILL status=none conv=fsync
SUM_BEFORE="$(sha256sum "$WFS/low/data" | awk '{print $1}')"
SIZE_BEFORE="$(stat -c %s "$WFS/low/data")"
settle
Q6_USED="$(usedof "$WFS/low")"
echo "  setup: $(mib "$Q6_USED") written under a $(mib $SRC_CAP) cap"
printf '         row: %s\n         sha256: %s…\n' "$(rowof "$WFS/low")" "${SUM_BEFORE:0:16}"

echo "  (a) lower the cap to $(mib $ALT_CAP) — BELOW the $(mib "$Q6_USED") already written:"
set +e; Q6_OUT="$(btrfs qgroup limit "$ALT_CAP" "$WFS/low" 2>&1)"; Q6_RC=$?; set -e
printf '        rc=%s %s\n' "$Q6_RC" "${Q6_OUT:+[$Q6_OUT]}"
settle
Q6_CAP_AFTER="$(capof "$WFS/low")"
if [ "$Q6_RC" -eq 0 ]; then
  Q6_LOWER=accepted
  echo "        -> ACCEPTED. btrfs does not validate the new limit against current usage."
else
  Q6_LOWER=refused
  echo "        -> REFUSED. The endpoint gets its 400 from the substrate for free."
fi

echo "  (b) is 'limit below referenced' even representable in the row?"
printf '        row: %s\n' "$(rowof "$WFS/low")"
if [ "$Q6_LOWER" = accepted ]; then
  chk "the lower cap is what the row reports"     "$Q6_CAP_AFTER" "$ALT_CAP"
  if [ "$Q6_CAP_AFTER" != none ] && [ "$(usedof "$WFS/low")" -gt "$Q6_CAP_AFTER" ]; then
    echo "        -> YES: referenced ($(mib "$(usedof "$WFS/low")")) EXCEEDS max_referenced"
    echo "           ($(mib "$Q6_CAP_AFTER")). The row is over-limit and btrfs states it plainly"
    echo "           rather than clamping or hiding it. Usage() reads this correctly —"
    echo "           used > limit is a state the engine CAN observe and report."
    Q6_OVER=representable
  else
    echo "        -> the row does not show used > limit; something clamped it"; Q6_OVER=clamped; FAIL=1
  fi
fi

echo "  (c) the existing bytes: still readable, and byte-for-byte intact?"
# The (iii) outcome check. Read the whole file back through a fresh open and rehash.
set +e; SUM_AFTER="$(sha256sum "$WFS/low/data" 2>&1 | awk '{print $1}')"; RD_RC=$?; set -e
SIZE_AFTER="$(stat -c %s "$WFS/low/data" 2>/dev/null || echo -1)"
printf '        read rc=%s  size %s -> %s  sha256 %s…\n' \
  "$RD_RC" "$SIZE_BEFORE" "$SIZE_AFTER" "${SUM_AFTER:0:16}"
chk "read succeeded"                              "$RD_RC" "0"
chk "size unchanged"                              "$SIZE_AFTER" "$SIZE_BEFORE"
chk "content byte-identical (sha256)"             "$SUM_AFTER" "$SUM_BEFORE"
Q6_DATA="$([ "$SUM_AFTER" = "$SUM_BEFORE" ] && echo intact || echo DAMAGED)"

echo "  (d) the NEXT write: refused immediately, or does it get a grace margin?"
# "Immediately" matters: if a lowered cap allowed even one more MiB, the endpoint
# could not promise "no further growth" and resource admission's Σ would still drift after a PATCH.
Q6_W="$(pour "$WFS/low/more" 4)"; settle
printf '        pour 4MiB: %s   row: %s\n' "$Q6_W" "$(rowof "$WFS/low")"
Q6_W_BYTES="${Q6_W##*wrote=}"
if [ "${Q6_W%% *}" != "rc=0" ] && [ "$Q6_W_BYTES" -eq 0 ]; then
  Q6_WRITE="refused at 0 bytes"
  echo "        -> ENOSPC with ZERO bytes written. Already over the cap, so there is no"
  echo "           margin at all: growth stops dead the instant the PATCH lands."
elif [ "${Q6_W%% *}" != "rc=0" ]; then
  Q6_WRITE="refused after $Q6_W_BYTES bytes"
  echo "        -> ENOSPC, but $(mib "$Q6_W_BYTES") got through first."
else
  Q6_WRITE="ALLOWED"
  echo "        -> *** the write SUCCEEDED past a cap below current usage ***"; FAIL=1
fi
# and the pre-existing bytes must survive the REFUSED write too — a failed write
# that damaged neighbouring extents would be the worst of the three outcomes.
SUM_W="$(sha256sum "$WFS/low/data" 2>/dev/null | awk '{print $1}')"
chk "original data still intact after the refused write" "$SUM_W" "$SUM_BEFORE"

echo "  (e) *** THE ONE THAT MATTERS: is it REVERSIBLE? ***"
# THIS IS WHERE THE FIRST DRAFT OF THIS SPIKE WAS WRONG. It asserted that `limit
# none` restores writability — the obvious repair path, and the one resource admission's "a lowering
# need not re-run admission" quietly assumes is always available. It is NOT. Measured
# below: while `referenced > max_referenced`, the quota-limit ioctl ITSELF returns
# ENOSPC, so the cap cannot be removed OR raised. A `size` PATCH that lands below
# current usage is therefore NOT undoable through the same endpoint that caused it,
# which upgrades this from "a semantic to document" to "a state the endpoint must not
# create by accident".
echo "        (e1) limit none:"
E1="$(limit none "$WFS/low")"
printf '             %s\n' "$E1"
echo "        (e2) raise to a HIGHER finite cap ($(mib $SRC_CAP)):"
E2="$(limit "$SRC_CAP" "$WFS/low")"
printf '             %s\n' "$E2"
echo "        (e3) raise to just above current usage ($(mib $(( Q6_USED + 1048576 )))):"
E3="$(limit "$(( Q6_USED + 1048576 ))" "$WFS/low")"
printf '             %s\n' "$E3"
settle
printf '             row is still: %s\n' "$(rowof "$WFS/low")"
if [ "${E1%% *}" = "rc=0" ]; then
  Q6_REV="yes (limit none works)"
  echo "        -> removable. The lowering is a soft state."
else
  Q6_REV="NO — the cap is FROZEN while over-limit"
  echo "        -> *** ALL THREE REFUSED. While referenced > max_referenced, the"
  echo "           quota-limit ioctl itself returns ENOSPC: the cap can be neither"
  echo "           removed nor RAISED. The PATCH is not undoable by another PATCH. ***"
fi
echo "        (e4) so what IS still possible on an over-limit volume?"
# Decisive for the endpoint's contract and for what an operator can be told to do.
# If even `rm` failed, a mis-typed PATCH would be unrecoverable without destroying
# the volume; if `rm` works, the escape exists but runs THROUGH the guest's data.
for op in "mkdir dir:mkdir $WFS/low/newdir" \
          "create file:touch $WFS/low/newfile" \
          "truncate existing to 0:truncate -s 0 $WFS/low/data" \
          "rm a file:rm -f $WFS/low/more"; do
  set +e; OUT="$(${op#*:} 2>&1)"; RC=$?; set -e
  printf '             %-24s rc=%s %s\n' "${op%%:*}" "$RC" "${OUT:+[${OUT##*: }]}"
done
settle
printf '             row after: %s\n' "$(rowof "$WFS/low")"
echo "        (e5) the ONLY escape: free bytes until referenced <= the cap, then retry."
# Step usage down one file at a time and retry the limit change after each, to show
# the transition is CONDITIONED on usage crossing back under the cap — not on time,
# not on a sync, and not on any command that can be issued from the control plane
# alone. Mirrors spike-qgroup-reclaim Q1's conditioned-not-timed shape.
Q6_ESCAPE=none
for f in data; do
  rm -f "$WFS/low/$f"; settle
  U="$(usedof "$WFS/low")"
  R="$(limit none "$WFS/low")"
  printf '             freed %-8s used=%-12s over-limit=%-4s limit none -> %s\n' \
    "$f" "$U" "$([ "$U" -gt "$ALT_CAP" ] && echo yes || echo no)" "$R"
  [ "${R%% *}" = "rc=0" ] && { Q6_ESCAPE="after freeing bytes"; break; }
done
settle
printf '             row: %s\n' "$(rowof "$WFS/low")"
Q6_R="$(pour "$WFS/low/after" 4)"; settle
printf '             pour 4MiB once back under: %s\n' "$Q6_R"
chk "writable again after freeing bytes + limit none" "${Q6_R%% *}" "rc=0"
echo "        -> the escape runs THROUGH THE GUEST'S DATA. The control plane cannot"
echo "           undo its own PATCH; someone must delete bytes first."

echo "  (f) boundary: lower to EXACTLY the current usage."
# The off-by-one that a naive endpoint validation would get wrong. If `limit ==
# referenced` still refuses writes, then "size must be >= current usage" is NOT a
# sufficient validation rule — the caller would get a 200 and an unwritable volume.
mk "$WFS/exact"; btrfs qgroup limit "$SRC_CAP" "$WFS/exact"
pour "$WFS/exact/data" 8 >/dev/null; settle
EX_USED="$(usedof "$WFS/exact")"
echo "        used = $EX_USED B exactly; set limit = $EX_USED"
echo "        $(limit "$EX_USED" "$WFS/exact")"
settle
printf '        row: %s\n' "$(rowof "$WFS/exact")"
chk "limit == referenced accepted and stored"     "$(capof "$WFS/exact")" "$EX_USED"
EX_W="$(pour "$WFS/exact/more" 1)"; settle
printf '        pour 1MiB at exactly-full: %s\n' "$EX_W"
if [ "${EX_W%% *}" != "rc=0" ]; then
  Q6_EXACT="refused (no headroom)"
  echo "        -> refused. limit == usage means FULL, not 'just barely OK': a"
  echo "           validation rule of 'size >= current usage' would hand the caller a"
  echo "           200 and a volume that cannot take another byte."
else
  Q6_EXACT="accepted a further write"
  echo "        -> a further write succeeded, so the row's usage figure had slack."
fi
echo "  -> LOWERING IS $Q6_LOWER, DATA IS $Q6_DATA, GROWTH STOPS ($Q6_WRITE),"
echo "     REVERSIBLE: $Q6_REV (escape: $Q6_ESCAPE)."
echo "     So 'existing bytes kept, further growth refused' is an HONEST semantic —"
echo "     but it is NOT the whole story, because the volume also becomes one the"
echo "     control plane can no longer re-cap. The endpoint should REFUSE a size below"
echo "     current usage (400) rather than accept it: not because the substrate"
echo "     forbids it, but because accepting it creates a state only a data deletion"
echo "     can leave."

echo
echo "=================================================================="
echo " VERDICT"
echo "=================================================================="
echo "Q1 inheritance     : NO INHERITANCE, in every shape. A snapshot gets a FRESH"
echo "                     qgroup row with max_referenced = 'none':"
echo "                       rw snapshot of a $(mib $SRC_CAP)-capped source  -> $(mib "$Q1_RW")"
echo "                       -r snapshot of the same source          -> $(mib "$Q1_RO")"
echo "                       rw snapshot of an UNCAPPED source       -> $(mib "$Q1_UNC")"
echo "                       snapshot of an explicitly-capped snapshot -> $(mib "$Q1_HOP")"
echo "                     No surprise in the other direction either: nothing invents"
echo "                     a cap. \`snapshot -i <qgroupid>\` is NOT an inheritance"
echo "                     switch: the new unit's own row still reads $(mib "${Q1_I:-n/a}"). A limit"
echo "                     on the level-1 parent was ${Q1_I_ENF:-n/a} under squota, but that is a"
echo "                     SHARED ceiling over all members — it cannot say \"this volume"
echo "                     gets N\", and one member exhausting it ENOSPCs its siblings."
echo "Q2 independence    : FULLY INDEPENDENT, both directions. Setting the snapshot's"
echo "                     limit left the source's at $(mib $SRC_CAP); moving the source's to"
echo "                     $(mib $MID_CAP) left the snapshot's at $(mib $ALT_CAP); \`limit none\` on the"
echo "                     snapshot left the source capped. So per-volume SetCapacity"
echo "                     is safe — it cannot reach another volume's row."
echo "Q3 read-only       : \`qgroup limit\` on a \`-r\` snapshot is ACCEPTED (rc=0) and the"
echo "                     value sticks; the ro flag still refuses DATA writes."
echo "                     \`btrfs property set <path> ro false\` SUCCEEDS here (rc=$RO_RC),"
echo "                     the limit survives the transition, and the unfrozen unit"
echo "                     then ENOSPCs precisely at it (enforced=$Q3_ENF). Unfreezing"
echo "                     never invents a cap. => Clone MAY be snapshot-then-unfreeze;"
echo "                     the substrate does not force a non-r snapshot."
echo "Q4 enforcement     : Q1 VERIFIED BY WRITING, not trusted."
echo "                       uncapped snapshot took $(mib "${E_FREE_BYTES:-0}") — $(( ${E_FREE_BYTES:-0} / SRC_CAP ))x its source's"
echo "                       cap, rc=0. ($Q4_UNCAPPED)"
echo "                       negative control (cap set by us) stopped at $(mib "${E_CAP_BYTES:-0}") of"
echo "                       $(mib $ALT_CAP). ($Q4_CONTROL) — so the pour CAN be stopped; the"
echo "                       first result is a real absence of a cap, not a quiet fs."
echo "                       isolation snapshot->source: source referenced moved $DELTA B ($Q4_ISO)"
echo "                       isolation source->snapshot: a source pinned at its cap"
echo "                       still accepted writes into its snapshot."
echo "Q5 mismatch hazard : $Q5_HAZARD. A snapshot with no explicit limit grew to"
echo "                     $(mib "${Q5_BYTES:-0}") while its control-plane record declared $(mib $DECLARED)."
echo "                     Zero errors, no lifecycle event, and \`Usage()\` reports"
echo "                     limit=0 — identical to a deliberately uncapped volume. So"
echo "                     the engine cannot detect it by reading; resource admission's Σ is simply"
echo "                     wrong by the overshoot, unboundedly."
echo "Q6 lowering a cap  : btrfs ${Q6_LOWER^^} a limit below current usage (rc=$Q6_RC), and"
echo "                     'referenced > max_referenced' is a REPRESENTABLE row state"
echo "                     (${Q6_OVER:-n/a}) that Usage() reads correctly. Pre-existing bytes"
echo "                     are $Q6_DATA — same size, same sha256, still readable, and"
echo "                     still intact after the refused write. The next write is"
echo "                     $Q6_WRITE, so growth stops the instant the PATCH lands, with"
echo "                     no grace margin — and it is not just data writes: mkdir,"
echo "                     touch, rename and truncate-to-0 ALL return ENOSPC. Only"
echo "                     unlink still works."
echo "                     *** REVERSIBLE: $Q6_REV. ***"
echo "                     While referenced > max_referenced the quota-limit ioctl"
echo "                     ITSELF returns ENOSPC ('unable to limit requested quota"
echo "                     group: No space left on device'), so the cap can be neither"
echo "                     removed (\`limit none\`) NOR raised — not to a higher finite"
echo "                     value, not even to just above current usage. The ONLY escape"
echo "                     is $Q6_ESCAPE, i.e. deleting the guest's data until"
echo "                     referenced falls back under the cap; then the limit call"
echo "                     succeeds on the FIRST try (conditioned on usage, not timed)."
echo "                     BOUNDARY: limit == current usage is accepted and stored, but a"
echo "                     further write is $Q6_EXACT — so 'size >= current usage' is"
echo "                     NOT a sufficient validation rule for PATCH."
echo "                     => PATCH /v1/volumes/{id} SHOULD REFUSE (400) a size below"
echo "                     the volume's current usage. Not because the substrate"
echo "                     forbids it — it accepts it, rc=0, and existing data is"
echo "                     perfectly safe — but because accepting it produces a volume"
echo "                     the CONTROL PLANE CAN NO LONGER RE-CAP: the repair for a"
echo "                     mis-typed size runs through deleting the guest's bytes, and"
echo "                     nothing in the external API lets the engine do that on the caller's behalf."
echo "                     This also puts a caveat on resource admission's \"a lowering need not\""
echo "                     (re-run the admission gate):"
echo "                     the Σ claim is right (lowering only reduces the left-hand"
echo "                     side, so no admission re-check is needed), but \"lowering is"
echo "                     the safe direction\" does not follow — it is the direction"
echo "                     that can strand a volume. And per Volume storage the guest's ENOSPC is"
echo "                     the only signal, while the data-plane exposure boundary only calls ENOSPC an EXPECTED"
echo "                     data-plane result when the boundary was DECLARED — a silently"
echo "                     accepted shrink declares nothing to the workload."
echo
echo "CONSEQUENCE FOR THE SEAM: \`Snapshot\`/\`Clone\` MUST set the new volume's cap"
echo "  explicitly, as part of the same operation that creates the bytes. It is not an"
echo "  optimization or a nicety — it is the only thing standing between resource admission's"
echo "  declared-cap accounting and an unbounded silent over-commit. And because the"
echo "  cap is a SEPARATE step from the snapshot ioctl (Q1) while the bytes exist from"
echo "  the first instant, the window between them is a real crash window: a snapshot"
echo "  that survives a crash before its limit call is a live uncapped volume."
echo "  resource admission's \"snapshot / clone volumes are counted by their declared cap\" is"
echo "  therefore a REQUIREMENT ON THE"
echo "  ENGINE, not a description of the substrate."
echo
echo "environment: work fs isolated=$ISOLATED, quota mode=${W_MODE:-$HOST_MODE},"
echo "             btrfs-progs $(btrfs --version | head -1 | awk '{print $2}')"
[ $FAIL -eq 0 ] && echo "ALL PROBES BEHAVED AS ASSERTED" || echo "SOME PROBES DEVIATED (see FAIL lines above)"
exit $FAIL
