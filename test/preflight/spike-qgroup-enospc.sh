#!/usr/bin/env bash
# spike-qgroup-enospc.sh — M6 Spike 3: does a btrfs qgroup limit produce a clean,
# guest-visible ENOSPC through the WHOLE real stack —
#   btrfs subvolume (qgroup limit) → overlayfs upper+work inside it
#   → runsc --overlay2=none --file-access=shared gofer → sentry → guest process?
#
# Prior work (deep-dives/volume.md D7, rootfs-overlay D6) only measured a qgroup
# limit on a BARE subvolume. This spike adds the untested chain: overlayfs on top,
# then the gofer/sentry, and answers five things:
#   Q1 plain write to cap  → exact guest errno
#   Q2 does the cap bind at the right number (qgroup show vs bytes written)
#   Q3 copy-up mid-way ENOSPC → does a failed copy-up CORRUPT the merged view?
#   Q4 bypasses: work dir, sparse/ftruncate, fallocate, hardlink, rename-from-lower
#   Q5 does the container survive hitting the cap?
#
# TWO ARMS, because btrfs has two quota modes and they answer Q3 differently:
#   A) the machine fs (/dev/vdb1) — OrbStack enabled quotas in **squota** (simple)
#      mode fs-wide; BTRFS_IOC_QUOTA_CTL is refused (EPERM) inside this machine,
#      so the mode cannot be changed here.
#   B) a PRIVATE loop btrfs formatted `mkfs.btrfs -O quota` → **full qgroup** mode
#      (pre-enabling at mkfs time sidesteps the blocked ioctl). Block-backed, not
#      tmpfs, so overlay upper/work and qgroup accounting are real.
# Each arm runs the same phases with lower on the SAME fs (overlayfs copy-up is a
# btrfs reflink → costs no data space) and, for Q3, a lower on the OTHER fs (copy-up
# must be a real data copy → actually runs out mid-way).
#
# Guest probe = the static `qgprobe` binary (test/preflight/qgprobe), not dd: the
# answers are exact errnos and exact byte counts, and the minimal rootfs has no
# coreutils (Spike 1 lesson: a missing binary masquerades as a real failure).
#
# Usage: sudo ./spike-qgroup-enospc.sh     (needs root: btrfs ioctls, mount, gofer)
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
# /var/tmp is native btrfs on this machine; /tmp is tmpfs and would void the spike.
WORK="$(mktemp -d /var/tmp/spike-qgroup.XXXXXX)"
RUNSC_ROOT="$WORK/runsc-root"
QGPROBE="$WORK/qgprobe"
CAP_MIB=64
BIG_MIB=40
PAD_MIB=54          # leaves ~10 MiB headroom under a 64 MiB cap
TAG="qgs$$"
FINDINGS="$WORK/findings"

MOUNTS=()           # overlay/loop mountpoints to umount, LIFO
SUBVOLS=()          # "fsroot|path|id" of subvolumes we created (and only ours)
CIDS=()

cleanup() {
  set +e
  for cid in "${CIDS[@]:-}"; do runsc --root="$RUNSC_ROOT" delete --force "$cid" >/dev/null 2>&1; done
  for ((i=${#MOUNTS[@]}-1; i>=0; i--)); do umount "${MOUNTS[i]}" >/dev/null 2>&1; done
  for entry in "${SUBVOLS[@]:-}"; do
    IFS='|' read -r fsroot path id <<<"$entry"
    btrfs subvolume delete "$path" >/dev/null 2>&1
    # deleting a subvol can leave a stale qgroup behind; drop ours explicitly
    [ -n "$id" ] && btrfs qgroup destroy "0/$id" "$fsroot" >/dev/null 2>&1
  done
  losetup -j "$WORK/loop.img" 2>/dev/null | cut -d: -f1 | while read -r d; do losetup -d "$d" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
trap cleanup EXIT

note() { echo "$*" >>"$FINDINGS"; }
raw()  { btrfs qgroup show -re --raw --sync "$1" 2>/dev/null | awk -v id="0/$2" '$1==id {print "referenced="$2" exclusive="$3" max_ref="$4}'; }

# mksubvol <fsroot> <path> <capMiB> -> echoes subvol id
mksubvol() {
  local fsroot="$1" path="$2" cap="$3" id
  btrfs subvolume create "$path" >/dev/null
  id="$(btrfs subvolume show "$path" | awk '/Subvolume ID/{print $3}')"
  SUBVOLS+=("$fsroot|$path|$id")
  btrfs qgroup limit "${cap}M" "$path"
  echo "$id"
}

# mkoverlay <lowerdir> <subvol> <merged>
mkoverlay() {
  local lower="$1" sv="$2" merged="$3"
  mkdir -p "$sv/upper" "$sv/work" "$merged"
  mount -t overlay "ovl-$TAG-$RANDOM" -o "lowerdir=$lower,upperdir=$sv/upper,workdir=$sv/work" "$merged"
  MOUNTS+=("$merged")
}

# launch <merged-rootfs> <cid>  — detached container, qgprobe idle as PID1
launch() {
  # NB: bash evaluates ALL rhs of a multi-var `local` before binding any of them,
  # so under `set -u` a later var may not reference an earlier one — declare apart.
  local root="$1" cid="$2"
  local bundle="$WORK/bundle-$cid"
  mkdir -p "$bundle"
  cat >"$bundle/config.json" <<JSON
{"ociVersion":"1.0.0",
 "process":{"terminal":false,"user":{"uid":0,"gid":0},
   "args":["/usr/bin/qgprobe","idle"],
   "env":["PATH=/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root"],"cwd":"/",
   "capabilities":{"bounding":["CAP_DAC_OVERRIDE"],"effective":["CAP_DAC_OVERRIDE"],"permitted":["CAP_DAC_OVERRIDE"]}},
 "root":{"path":"$root","readonly":false},"hostname":"guest",
 "mounts":[{"destination":"/proc","type":"proc","source":"proc"},
   {"destination":"/dev","type":"tmpfs","source":"tmpfs","options":["nosuid","mode=755","size=65536k"]}],
 "linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON
  CIDS+=("$cid")
  # detached stdio MUST go to a real file, never a pipe (see memory: runsc-detach-stdio-deadlock)
  runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
    --overlay2=none --file-access=shared --systemd-cgroup=false \
    run --detach --bundle="$bundle" "$cid" >"$bundle/guest.log" 2>&1
  for _ in $(seq 40); do
    [ "$(runsc --root="$RUNSC_ROOT" state "$cid" 2>/dev/null | awk -F'"' '/"status"/{print $4}')" = "running" ] && return 0
    sleep 0.25
  done
  echo "launch: container $cid did not reach running"; cat "$bundle/guest.log"; return 1
}

# g <cid> <probe args...> — run a probe in the live guest
g() { runsc --root="$RUNSC_ROOT" --platform=systrap --network=none --overlay2=none \
        --file-access=shared --systemd-cgroup=false exec "$1" /usr/bin/qgprobe "${@:2}" 2>&1; }

kv() { echo "$1" | awk -F= -v k="$2" '$1==k{print $2; exit}'; }

# whichever errno a `fill` actually reported: it prints OPEN_ERRNO when the open
# itself was refused, WRITE_ERRNO when the open succeeded and a write was refused.
fillerr() {
  local o w
  o="$(kv "$1" OPEN_ERRNO)"; w="$(kv "$1" WRITE_ERRNO)"
  if [ -n "$o" ] && [ "$o" != 0 ]; then echo "at-open errno=$o"
  elif [ -n "$w" ]; then echo "at-write errno=$w"
  else echo "no errno reported"; fi
}

# =============================================================================
# arm <label> <fsroot> <mode> <same-fs-template> <other-fs-template>
#   fsroot            = btrfs mount whose subvolumes get the qgroup cap
#   same-fs-template  = lower on fsroot         → overlayfs copy-up CAN reflink
#   other-fs-template = lower on the OTHER fs   → copy-up must be a real data copy
# Both lowers are full guest Templates (the guest must be able to exec
# /usr/bin/qgprobe out of whichever lower it is booted on).
# =============================================================================
arm() {
  local label="$1" fsroot="$2" mode="$3" tmpl="$4" xlower="$5"
  echo; echo "############ ARM $label  (quota mode = $mode, fs = $fsroot) ############"
  local LOWMD5; LOWMD5="$(md5sum "$tmpl/big" | cut -d' ' -f1)"
  echo "== [$label] lowers: same-fs=$tmpl  other-fs=$xlower =="
  echo "   lower /big md5=$LOWMD5 size=$((BIG_MIB<<20))"

  # ---------------- P1: plain write to cap (Q1, Q2, Q5) ----------------
  echo; echo "== [$label] P1: guest fills its writable overlay until the ${CAP_MIB}MiB cap binds =="
  local sv1="$fsroot/sv1-$TAG" id1 m1="$WORK/m1-$label" c1="${TAG}p1${label}"
  id1="$(mksubvol "$fsroot" "$sv1" $CAP_MIB)"
  mkoverlay "$tmpl" "$sv1" "$m1"
  echo "   qgroup 0/$id1 before: $(raw "$fsroot" "$id1")"
  launch "$m1" "$c1"
  local O; O="$(g "$c1" fill /fill 1024)"
  echo "$O" | sed 's/^/   /'
  sync
  local BYTES ERRNO ERRSTR
  BYTES="$(kv "$O" FILL_BYTES)"; ERRNO="$(kv "$O" WRITE_ERRNO)"; ERRSTR="$(kv "$O" WRITE_ERRSTR)"
  echo "   qgroup 0/$id1 after:  $(raw "$fsroot" "$id1")"
  note "[$label/Q1] guest plain-write hit cap: errno=$ERRNO ($ERRSTR) after FILL_BYTES=$BYTES"
  note "[$label/Q2] cap=$((CAP_MIB<<20)) vs guest-written=$BYTES vs qgroup: $(raw "$fsroot" "$id1")"

  echo "== [$label] P1/Q5: is the container still alive after ENOSPC? =="
  local L; L="$(g "$c1" echo ALIVE)"
  echo "$L" | sed 's/^/   /'
  local S; S="$(runsc --root="$RUNSC_ROOT" state "$c1" 2>/dev/null | awk -F'"' '/"status"/{print $4}')"
  echo "   runsc state=$S"
  local R2; R2="$(g "$c1" fill /fill2 1024)"
  note "[$label/Q5] after cap: exec ok=$(kv "$L" ECHO) state=$S; a second write still fails cleanly ($(fillerr "$R2"))"

  echo "== [$label] P1/Q4: is the overlay work dir reachable from inside the guest? =="
  local W; W="$(g "$c1" ls /)"
  echo "   guest / entries: $(echo "$W" | grep -c '^ENT=') (work/upper must NOT appear)"
  echo "$W" | grep -qE '^ENT=(work|upper)$' && note "[$label/Q4] LEAK: work/upper visible in guest" \
                                            || note "[$label/Q4] work+upper NOT visible in guest merged view (same subvol, same qgroup 0/$id1)"

  # ---------------- P2: copy-up mid-way ENOSPC (Q3) ----------------
  # 2a same-fs lower  → overlayfs copy-up is a btrfs reflink
  # 2b other-fs lower → copy-up must be a real data copy, runs out mid-way
  local variant
  for variant in samefs otherfs; do
    local low="$tmpl"; [ "$variant" = otherfs ] && low="$xlower"
    local lmd5; lmd5="$(md5sum "$low/big" | cut -d' ' -f1)"
    echo; echo "== [$label] P2/$variant: copy-up a ${BIG_MIB}MiB lower file with only ~$((CAP_MIB-PAD_MIB))MiB headroom =="
    local sv2="$fsroot/sv2$variant-$TAG" id2 m2="$WORK/m2-$label-$variant" c2="${TAG}p2$variant$label"
    id2="$(mksubvol "$fsroot" "$sv2" $CAP_MIB)"
    mkdir -p "$sv2/upper" "$sv2/work"
    dd if=/dev/zero of="$sv2/upper/pad" bs=1M count=$PAD_MIB status=none
    sync
    mkoverlay "$low" "$sv2" "$m2"
    echo "   qgroup 0/$id2 pre-copy-up: $(raw "$fsroot" "$id2")"
    launch "$m2" "$c2"
    local P; P="$(g "$c2" poke /big)"
    echo "$P" | sed 's/^/   /'
    sync
    echo "   qgroup 0/$id2 post-copy-up: $(raw "$fsroot" "$id2")"
    echo "   -- guest reads the file back in full (truncation/corruption check) --"
    local M; M="$(g "$c2" md5 /big)"
    echo "$M" | sed 's/^/   /'
    local GSIZE GMD5; GSIZE="$(kv "$M" MD5_READBYTES)"; GMD5="$(kv "$M" MD5_SUM)"
    echo "   -- host: upper dir contents + any partial in work --"
    ls -l "$sv2/upper/" | sed 's/^/   /'
    find "$sv2/work" -mindepth 1 -printf '   work: %y %s %p\n' 2>/dev/null || true
    local UPSZ="absent"; [ -f "$sv2/upper/big" ] && UPSZ="$(stat -c %s "$sv2/upper/big")"
    local SHARED="n/a"; [ -f "$sv2/upper/big" ] && SHARED="$(filefrag -v "$sv2/upper/big" 2>/dev/null | grep -c shared)"

    local verdict
    if [ "$(kv "$P" OPEN_ERRNO)" != 0 ] || [ "$(kv "$P" WRITE_ERRNO)" != 0 ]; then
      if [ "$GSIZE" = "$((BIG_MIB<<20))" ] && [ "$GMD5" = "$lmd5" ]; then
        verdict="copy-up REFUSED (open errno=$(kv "$P" OPEN_ERRNO)/write errno=$(kv "$P" WRITE_ERRNO)) and merged view INTACT (size=$GSIZE md5==lower); upper/big=$UPSZ"
      else
        verdict="CORRUPTION: copy-up failed AND merged view changed (size=$GSIZE md5=$GMD5 vs lower $lmd5); upper/big=$UPSZ"
      fi
    else
      if [ "$GSIZE" = "$((BIG_MIB<<20))" ]; then
        verdict="copy-up SUCCEEDED despite headroom < file size (reflink; shared-extents=$SHARED); merged size=$GSIZE, upper/big=$UPSZ; qgroup now $(raw "$fsroot" "$id2")"
      else
        verdict="CORRUPTION: copy-up reported OK but merged view is $GSIZE bytes (lower is $((BIG_MIB<<20)))"
      fi
    fi
    note "[$label/Q3/$variant] $verdict"

    # follow-on: after the copy-up outcome, can the guest still write at all?
    local F3; F3="$(g "$c2" fill /after 1024)"
    note "[$label/Q3/$variant] remaining headroom after the copy-up outcome: a fresh fill got $(kv "$F3" FILL_BYTES) bytes, then $(fillerr "$F3")"
  done

  # ---------------- P3: bypass attempts (Q4) ----------------
  echo; echo "== [$label] P3: bypass attempts — sparse / fallocate / holes / hardlink / rename =="
  local sv3="$fsroot/sv3-$TAG" id3 m3="$WORK/m3-$label" c3="${TAG}p3${label}"
  id3="$(mksubvol "$fsroot" "$sv3" $CAP_MIB)"
  mkoverlay "$tmpl" "$sv3" "$m3"
  launch "$m3" "$c3"

  local SP; SP="$(g "$c3" sparse /sparsefile $((CAP_MIB*8)))"      # ftruncate to 8x the cap
  echo "$SP" | sed 's/^/   sparse: /'; sync
  # The guest's own st_blocks is NOT the truth: the sentry synthesizes it from the
  # size, so compare it against the host's real allocation for the same inode.
  local HOSTALLOC; HOSTALLOC="$(stat -c %b "$sv3/upper/sparsefile" 2>/dev/null || echo ?)"
  note "[$label/Q4/sparse] ftruncate to $((CAP_MIB*8))MiB (8x the cap): errno=$(kv "$SP" TRUNCATE_ERRNO) apparent=$(kv "$SP" SPARSE_SIZE); guest-reported allocated=$(kv "$SP" SPARSE_ALLOCBYTES) but host st_blocks=$HOSTALLOC → sentry FABRICATES st_blocks; real cost is the qgroup: $(raw "$fsroot" "$id3")"

  local HO; HO="$(g "$c3" holes /sparsefile 1 $((CAP_MIB*4)))"     # 1 byte every 1 MiB
  echo "$HO" | sed 's/^/   holes: /'; sync
  note "[$label/Q4/holes] 1B-per-MiB into the sparse file: written=$(kv "$HO" HOLES_WRITTEN)/$((CAP_MIB*4)) write_errno=$(kv "$HO" WRITE_ERRNO) fsync_errno=$(kv "$HO" FSYNC_ERRNO); qgroup $(raw "$fsroot" "$id3")"

  local FA; FA="$(g "$c3" falloc /fallocfile $((CAP_MIB*4)))"
  echo "$FA" | sed 's/^/   falloc: /'; sync
  note "[$label/Q4/fallocate] fallocate $((CAP_MIB*4))MiB: errno=$(kv "$FA" FALLOCATE_ERRNO) ($(kv "$FA" FALLOCATE_ERRSTR)) allocated=$(kv "$FA" FALLOC_ALLOCBYTES); qgroup $(raw "$fsroot" "$id3")"

  local LN; LN="$(g "$c3" link /big /hardbig)"
  echo "$LN" | sed 's/^/   link: /'; sync
  note "[$label/Q4/hardlink] link(lower /big → /hardbig): errno=$(kv "$LN" LINK_ERRNO) ($(kv "$LN" LINK_ERRSTR)); qgroup $(raw "$fsroot" "$id3")"

  local RN; RN="$(g "$c3" rename /big /renbig)"
  echo "$RN" | sed 's/^/   rename: /'; sync
  local RM; RM="$(g "$c3" md5 /renbig)"
  note "[$label/Q4/rename] rename(lower /big → /renbig): errno=$(kv "$RN" RENAME_ERRNO) ($(kv "$RN" RENAME_ERRSTR)); readback=$(kv "$RM" MD5_READBYTES) md5match=$([ "$(kv "$RM" MD5_SUM)" = "$LOWMD5" ] && echo yes || echo no); qgroup $(raw "$fsroot" "$id3")"

  echo "   -- final qgroup for the bypass subvol vs cap $((CAP_MIB<<20)) --"
  btrfs qgroup show -re --raw --sync "$fsroot" 2>/dev/null | awk -v id="0/$id3" '$1==id' | sed 's/^/   /'
  local REF; REF="$(raw "$fsroot" "$id3" | sed 's/.*referenced=\([0-9]*\).*/\1/')"
  note "[$label/Q4/total] bypass subvol referenced=$REF vs cap=$((CAP_MIB<<20)) → $([ "${REF:-0}" -le $((CAP_MIB<<20)) ] && echo "cap HELD" || echo "cap EXCEEDED")"
}

# =============================================================================
# main
# =============================================================================
[ "$(id -u)" = 0 ] || { echo "must run as root (btrfs ioctls + gofer)"; exit 1; }

echo "== substrate =="
echo "   kernel: $(uname -r)"
echo "   runsc:  $(runsc --version | head -1)"
echo "   btrfs-progs: $(btrfs --version | head -1)"
MACHINE_FS="$(findmnt -T "$WORK" -no TARGET)"
MACHINE_UUID="$(btrfs filesystem show "$MACHINE_FS" | grep -oE 'uuid: [0-9a-f-]+' | head -1 | cut -d' ' -f2)"
MACHINE_MODE="$(cat "/sys/fs/btrfs/$MACHINE_UUID/qgroups/mode" 2>/dev/null || echo unknown)"
echo "   machine fs $MACHINE_FS ($(findmnt -T "$WORK" -no SOURCE,FSTYPE)) quota mode=$MACHINE_MODE"
echo "   scratch: $WORK  (native btrfs, NOT tmpfs, NOT virtiofs)"
note "[substrate] kernel=$(uname -r) runsc=$(runsc --version | head -1 | awk '{print $3}') machine-fs quota mode=$MACHINE_MODE"

echo; echo "== build guest probe binary =="
( cd "$REPO/test/preflight/qgprobe" && CGO_ENABLED=0 GO111MODULE=off go build -o "$QGPROBE" . )
echo "   qgprobe: $("$QGPROBE" echo built)"

echo; echo "== arm B substrate: private loop btrfs with FULL qgroups (mkfs -O quota) =="
# BTRFS_IOC_QUOTA_CTL is EPERM inside this machine even as root with CAP_SYS_ADMIN
# (balance/scrub/label ioctls all work), so `btrfs quota enable` cannot be used;
# pre-enabling at mkfs time is the way to get full-qgroup mode here.
truncate -s 2G "$WORK/loop.img"
mkfs.btrfs -q -O quota -m single -d single "$WORK/loop.img"
LOOPMNT="$WORK/loopmnt"; mkdir -p "$LOOPMNT"
mount -o loop,noatime "$WORK/loop.img" "$LOOPMNT"
MOUNTS+=("$LOOPMNT")
LOOP_UUID="$(btrfs filesystem show "$LOOPMNT" | grep -oE 'uuid: [0-9a-f-]+' | head -1 | cut -d' ' -f2)"
LOOP_MODE="$(cat "/sys/fs/btrfs/$LOOP_UUID/qgroups/mode" 2>/dev/null || echo none)"
echo "   loop fs $(findmnt -no SOURCE "$LOOPMNT") quota mode=$LOOP_MODE"
[ "$LOOP_MODE" = qgroup ] || { echo "FATAL: loop fs is not in full-qgroup mode ($LOOP_MODE)"; exit 1; }
note "[substrate] arm-B loop fs quota mode=$LOOP_MODE (via mkfs -O quota; quota-ctl ioctl is EPERM on this machine)"

# One guest Template per fs, each with a BIG_MiB /big. Each arm uses the Template
# on its OWN fs as the same-fs lower (copy-up can reflink) and the Template on the
# other fs as the cross-fs lower (copy-up must really copy the bytes). Both must be
# bootable rootfs dirs, since the guest execs /usr/bin/qgprobe from the lower.
TMPL_MACHINE="$WORK/tmpl-machine"
TMPL_LOOP="$LOOPMNT/tmpl-loop"
echo; echo "== build one guest Template per fs (each with a ${BIG_MIB}MiB /big) =="
for pair in "$TMPL_MACHINE" "$TMPL_LOOP"; do
  "$REPO/test/preflight/build-rootfs.sh" "$pair" "$QGPROBE" >/dev/null
  cp -a "$QGPROBE" "$pair/usr/bin/qgprobe"
  dd if=/dev/urandom of="$pair/big" bs=1M count=$BIG_MIB status=none
  echo "   $pair  /big md5=$(md5sum "$pair/big" | cut -d' ' -f1)"
done
sync

arm MACHINE "$WORK"    "$MACHINE_MODE" "$TMPL_MACHINE" "$TMPL_LOOP"
arm LOOP    "$LOOPMNT" "$LOOP_MODE"    "$TMPL_LOOP"    "$TMPL_MACHINE"

echo; echo "=================== FINDINGS ==================="
cat "$FINDINGS"
echo "================================================"
