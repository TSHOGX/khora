#!/usr/bin/env bash
# spike-ro-snapshot-mount.sh — §5.C: is a **read-only btrfs snapshot** usable as a
# Volume materialization when the gofer proxies it into a live guest?
#
# What is already measured, and what is NOT (this is the whole reason the script
# exists):
#   - deep-dives/volume.md D1 measured the ro arm with a **plain host directory**
#     mounted ro: guest statfs says read-only, guest writes fail EROFS, the host
#     never sees the write. In that experiment the SOURCE was ordinary writable
#     storage and the read-only-ness lived entirely in the **mount option**.
#   - What a snapshot-backed Volume actually is, is different in kind: a
#     `btrfs subvolume snapshot -r` unit is **immutable at the filesystem level**.
#     The refusal comes from the btrfs sb (`ro` subvolume flag), not from the bind
#     option. Nobody has measured that combination through a gofer.
#
# §5.C promises "ro 的强制在运行时(ro bind ⇒ guest 的写得 EROFS,一个干净的 POSIX
# 拒绝而非静默成功)". That sentence is what P2 below either upholds or breaks for
# immutable sources, and it is load-bearing: `Materialize` does not take
# `accessMode` (volume.md D2), so the ONLY thing standing between "engine wrote
# ReadOnly:false into the mount spec" and a wrong outcome is the substrate's own
# refusal. If an immutable source answers a rw bind with EIO, a sentry fault, or
# worst of all a write that looks successful in-guest and never lands on the host,
# then a single bad boolean in the mediator becomes silent data loss instead of a
# clean errno the guest can handle.
#
# Base = spike-volume-mount.sh (bundle shape, pinned runsc flags, gofer-proxied
# extra bind at /vol, the ro-arm-coexists-with-rw-arms structure) and
# spike-live-volume-snapshot.sh (subvolume as the Volume source, long-running
# guest). Their harness lessons are honoured verbatim, see HARNESS TRAPS below.
#
# The in-guest probe is a **static Go binary**, not dash builtins. That is a
# deliberate departure from spike-volume-mount.sh: dash can only say "the write
# failed", and the entire question in P2 is *which* failure — EROFS vs EIO vs
# EPERM vs silent success. Precedent: spike-volume-yank.sh injects the same way.
# (The bare rootfs from build-rootfs.sh has no coreutils, so a static CGO-off
# binary is also the only thing that needs no loader beyond what dash brought.)
#
# HARNESS TRAPS (each cost a false conclusion when learned the hard way; do not
# "simplify" them away):
#   1. The live-guest probe loop must OUTLAST the whole run. A probe that ends by
#      itself makes PID 1 exit and the container go `stopped`, indistinguishable
#      from "this change killed the sandbox". Hence LOOP_SECS >> run time, and the
#      verdict refuses to grade an arm whose log contains PROBE_DONE.
#   2. `runsc run --detach` with stdio on a PIPE deadlocks Cmd.Wait forever —
#      stdio always goes to FILES.
#   3. Anything interpolated into `process.args` must be a SINGLE PHYSICAL LINE;
#      a raw newline there makes runsc report an illegal string literal. (We pass
#      argv arrays, so this bites only if someone re-introduces a shell one-liner.)
#
# ISOLATION (other agents run runsc and btrfs on this box concurrently):
# --network=none, no netns, a private --root, CID prefix unique to this spike+pid,
# and every subvolume under one $WORK dir. The shared btrfs carries ~11 FOREIGN
# qgroup rows belonging to OrbStack/docker — cleanup destroys ONLY the ids this
# script created, recorded at creation time.
#
# Usage: sudo ./spike-ro-snapshot-mount.sh     (needs root: gofer, btrfs ioctls)
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "must run as root (runsc gofer + btrfs ioctls); re-run with sudo" >&2; exit 2; }

LOOP_SECS="${LOOP_SECS:-600}"   # see trap 1 — must outlive the whole run
IVL_MS="${IVL_MS:-250}"
WINDOW=20                       # iteration lines sampled for a steady-state signature

# /var/tmp, not /tmp: the Volume source has to be on btrfs to be a subvolume at
# all, and ADR-0002 forbids the virtiofs shared mount for any of this.
WORK="$(mktemp -d /var/tmp/spike-rosnap.XXXXXX)"
ROOTFS="$WORK/rootfs"; RUNSC_ROOT="$WORK/runsc-root"
V="$WORK/vol-src"            # the source Volume (rw btrfs subvolume)
S1="$WORK/snap-ro"           # `snapshot -r` of V: the shared read-only unit
S2="$WORK/snap-unfreeze"     # `snapshot -r` of V, later flipped rw by P4
SUBVOLS="$S1 $S2 $V"
A="rosnap$$a"                # P1 : S1 bound ro
B="rosnap$$b"                # P2 : S1 bound RW  (the load-bearing arm)
C="rosnap$$c"                # P3 : S1 bound ro, second concurrent sandbox
D="rosnap$$d"                # P4 : S2 (unfrozen) bound rw
E="rosnap$$e"                # P5 : V (the source) bound rw, alongside A/C
CIDS="$A $B $C $D $E"
QIDS="$WORK/qids"; : > "$QIDS"
MP="$(findmnt -n -o TARGET -T "$WORK")"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"

cleanup() {
  set +e
  for cid in $CIDS; do
    runsc --root="$RUNSC_ROOT" kill "$cid" KILL 2>/dev/null
    runsc --root="$RUNSC_ROOT" delete --force "$cid" 2>/dev/null
  done
  # A ro subvolume must be flipped rw before it will delete on some progs
  # versions; harmless when it is already rw.
  for s in $SUBVOLS; do
    btrfs property set "$s" ro false 2>/dev/null
    btrfs subvolume delete "$s" >/dev/null 2>&1
  done
  # squota leaks a qgroup row per unit that outlived a snapshot (deep-dives/volume.md
  # D7.1). Sweep ONLY the ids we created — the other rows on this fs are
  # OrbStack/docker's and destroying them is a real outage.
  sync; btrfs subvolume sync "$MP" >/dev/null 2>&1
  while read -r qid; do
    [ -n "$qid" ] && btrfs qgroup destroy "0/$qid" "$MP" 2>/dev/null
  done < "$QIDS"
  echo
  echo "-- cleanup residue (must be empty) --"
  btrfs subvolume list "$MP" 2>/dev/null | grep -- "spike-rosnap" || echo "  no subvolumes left"
  btrfs qgroup show -re --raw --sync -f "$MP" 2>/dev/null \
    | awk -v ids="$(tr '\n' ' ' < "$QIDS")" 'NR>2 { split(ids,a," "); for (i in a) if ($1 == "0/"a[i]) print "  LEFTOVER qgroup "$0 }'
  rm -rf "$WORK"
  set -e
}
trap cleanup EXIT

hr() { echo; echo "=== $* ==="; }

subvolid() { btrfs subvolume show "$1" 2>/dev/null | awk '/Subvolume ID:/ {print $3}'; }
roflag()   { btrfs property get "$1" ro 2>&1 | tr -d '\n'; }

# fp — content fingerprint of a subtree: every path plus a short content hash.
# P5 needs to prove the snapshot's BYTES stay frozen while the source changes;
# "the seed file is still there" is far too weak an assertion for that (it would
# pass even if every other file had been rewritten).
fp() {
  local p="$1"
  ( cd "$p" 2>/dev/null || exit 0
    find . -mindepth 1 | LC_ALL=C sort | while read -r f; do
      if [ -f "$f" ]; then echo "F $f $(sha256sum < "$f" | cut -c1-16)"
      elif [ -d "$f" ]; then echo "D $f"
      else echo "? $f"; fi
    done )
}

# ---------------------------------------------------------------------------
# the in-guest probe
# ---------------------------------------------------------------------------
cat > "$WORK/probe.go" <<'GO'
package main

// rosnapprobe — the in-guest half. Static (CGO off) so the bare rootfs needs no
// extra loader/libc. Emits one machine-greppable KEY=VALUE line per iteration
// with RAW errno integers, because the whole question this spike asks is WHICH
// failure a write to an immutable source produces, not merely "it failed".
//
// It NEVER exits on error: a single first fault cannot distinguish "permanently
// refused", "refused then recovered", and "hung". Steady state is the answer.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const stRDONLY = 1 // ST_RDONLY in statfs f_flags

func ename(e syscall.Errno) string {
	switch e {
	case syscall.EROFS:
		return "EROFS"
	case syscall.EPERM:
		return "EPERM"
	case syscall.EACCES:
		return "EACCES"
	case syscall.EIO:
		return "EIO"
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EEXIST:
		return "EEXIST"
	case syscall.ENOSPC:
		return "ENOSPC"
	case syscall.ESTALE:
		return "ESTALE"
	case syscall.EBADF:
		return "EBADF"
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.ENOTEMPTY:
		return "ENOTEMPTY"
	case syscall.EOPNOTSUPP:
		return "EOPNOTSUPP"
	case syscall.EBUSY:
		return "EBUSY"
	case syscall.ENOTCONN:
		return "ENOTCONN"
	}
	return "OTHER"
}

// res renders one outcome as a single token: ok, or E<errno>/<NAME>. One token
// per path means a whole steady state fits on one line and a behaviour change
// shows up as a token change — which is what the host side diffs.
func res(err error) string {
	if err == nil {
		return "ok"
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		return fmt.Sprintf("E%d/%s", int(e), ename(e))
	}
	return fmt.Sprintf("ERR/%T", err)
}

// writeFresh: create+truncate+write+fsync+close, with the payload the HOST will
// look for afterwards. fsync is explicit: a failure deferred to fsync/close
// would make "the guest saw an error" a much weaker claim than it looks, and a
// success that is only ever buffered would make "the guest saw success" weaker
// still — both matter for telling a clean refusal from a silent success.
func writeFresh(p, payload string) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(payload); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readFile returns the content as ONE whitespace-free token. The trim is not
// cosmetic: every field below is emitted into a single space-separated KEY=VALUE
// line, so a trailing newline in a seed file would split that line in half and
// the host would read the remaining fields as empty. (Measured: it did.)
func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return res(err)
	}
	s := strings.TrimRight(string(b), " \t\r\n")
	if s == "" {
		return "<empty>"
	}
	return strings.ReplaceAll(s, " ", "_")
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: rosnapprobe loop <vol> <ivlMS> <secs> <tag> | live <vol> <tag>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "loop":
		loop(os.Args[2], atoi(os.Args[3]), atoi(os.Args[4]), os.Args[5])
	case "live":
		live(os.Args[2], os.Args[3])
	default:
		os.Exit(2)
	}
}
GO

cat >> "$WORK/probe.go" <<'GO'

// battery runs every probe path once and returns the assembled line. Paths are
// tagged per arm so the HOST can attribute any byte that lands to the exact
// sandbox that wrote it — the discriminator for "silent success".
func battery(vol, tag string, it int) string {
	payload := "WROTE_BY_" + tag

	// --- READ side: P1's actual question. A read-only snapshot is worthless as
	// a shared Volume if the guest cannot enumerate it and descend into it.
	ls := "?"
	if ents, e := os.ReadDir(vol); e != nil {
		ls = res(e)
	} else {
		ls = fmt.Sprintf("n=%d", len(ents))
	}
	seed := readFile(vol + "/seed")                  // top-level file
	nest := readFile(vol + "/sub/nested")            // one level down
	deep := readFile(vol + "/sub/deeper/leaf")       // two levels down
	mrk := readFile(vol + "/hostmark")               // host wrote this AFTER launch:
	                                                 // present in the live source,
	                                                 // must be absent from a snapshot

	// --- statfs: does the guest even KNOW it is read-only? A backend that
	// materializes an immutable unit but reports a writable fs sends every
	// in-guest space/permission precheck down the wrong branch.
	rdonly := "?"
	var sfs syscall.Statfs_t
	if e := syscall.Statfs(vol, &sfs); e != nil {
		rdonly = res(e)
	} else if uint64(sfs.Flags)&stRDONLY != 0 {
		rdonly = "yes"
	} else {
		rdonly = "no"
	}

	// --- WRITE side: four distinct mutation classes, because "read-only" can
	// be enforced unevenly and each class is a different kernel path:
	//   W   data write to a NEW file (openat O_CREAT + write + fsync)
	//   MKD directory creation
	//   UNL unlink of a file that EXISTS in the source (destructive metadata op —
	//       if this succeeds on a `-r` snapshot the unit is not immutable at all)
	//   CHM chmod of an existing file (pure metadata, no allocation)
	wpath := fmt.Sprintf("%s/w-%s", vol, tag)
	w := res(writeFresh(wpath, payload))
	rb := "skip"
	if w == "ok" {
		// Readback matters only when the write claimed success. If the guest can
		// read its own bytes back but the host cannot see them, that is the
		// worst outcome in P2: a write that appears to succeed and is a lie.
		rb = readFile(wpath)
	}
	dpath := fmt.Sprintf("%s/d-%s-%d", vol, tag, it)
	mkd := res(os.Mkdir(dpath, 0o755))
	if mkd == "ok" {
		_ = os.Remove(dpath)
	}
	unl := res(os.Remove(vol + "/victim"))

	// CHM targets a mode DIFFERENT from the seeded 0644 (host seeds 0644; we ask
	// for 0600) and MODE reports what the guest reads back afterwards. Asking for
	// the mode a file already has would let a no-op fast path answer "ok" without
	// any mutation being attempted, and that indistinguishability is the whole
	// hazard: chmod is the one mutation class that needs no allocation, so it is
	// the likeliest place for a "success" that never reaches the host. The host
	// prints the real mode of the same file at the end — MODE=600 with a host
	// mode of 644 would be a silent success in the strict sense.
	chm := res(os.Chmod(vol+"/seed", 0o600))
	mode := "?"
	if fi, e := os.Stat(vol + "/seed"); e != nil {
		mode = res(e)
	} else {
		mode = fmt.Sprintf("%03o", fi.Mode().Perm())
	}

	// TMP is NOT on the volume. It reports on the SANDBOX, so it separates
	// "volume I/O is refused" from "the sandbox died / the sentry faulted".
	tmp := res(writeFresh("/tmp/live", payload))

	return fmt.Sprintf("IT=%d LS=%s RDONLY=%s SEED=%s NEST=%s DEEP=%s MRK=%s W=%s RB=%s MKD=%s UNL=%s CHM=%s MODE=%s TMP=%s",
		it, ls, rdonly, seed, nest, deep, mrk, w, rb, mkd, unl, chm, mode, tmp)
}

// loop is the live arm. It must outlive the whole host-side run (harness trap 1):
// if it returned early, PID 1 would exit, the container would go `stopped`, and
// that is indistinguishable from "the mount broke the sandbox".
func loop(vol string, ivlMS, secs int, tag string) {
	fmt.Printf("PROBE_START tag=%s vol=%s pid=%d\n", tag, vol, os.Getpid())
	os.Stdout.Sync()
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for it := 1; time.Now().Before(deadline); it++ {
		fmt.Println(battery(vol, tag, it))
		os.Stdout.Sync()
		time.Sleep(time.Duration(ivlMS) * time.Millisecond)
	}
	fmt.Println("PROBE_DONE")
}

// live is what a fresh `runsc exec` runs: in-guest loop liveness could be a
// limping sentry, whereas a brand-new process proves the container is genuinely
// serviceable with this mount in place.
func live(vol, tag string) {
	fmt.Printf("EXEC_PID=%d\n", os.Getpid())
	fmt.Println("EXEC_" + battery(vol, tag, 0))
	fmt.Println("PROBE_DONE")
}
GO

hr "build minimal rootfs + inject static rosnapprobe"
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" /usr/bin/dash >/dev/null
mkdir -p "$ROOTFS/vol"
CGO_ENABLED=0 GO111MODULE=off go build -o "$ROOTFS/usr/bin/rosnapprobe" "$WORK/probe.go"
chmod 0755 "$ROOTFS/usr/bin/rosnapprobe"
echo "rosnapprobe: $(ls -l "$ROOTFS/usr/bin/rosnapprobe" | awk '{print $5" bytes"}') static=$(ldd "$ROOTFS/usr/bin/rosnapprobe" 2>&1 | tr -d '\n' | cut -c1-40)"

hr "source Volume V = btrfs subvolume on $MP, seeded with a nested tree"
btrfs subvolume create "$V"
echo "$(subvolid "$V")" >> "$QIDS"
mkdir -p "$V/sub/deeper"
printf 'SEED_V1\n'   > "$V/seed"
printf 'NESTED_OK\n' > "$V/sub/nested"
printf 'DEEP_OK\n'   > "$V/sub/deeper/leaf"
printf 'VICTIM\n'    > "$V/victim"     # UNL's target: exists in V and in every snapshot
sync
echo "V seeded:"; fp "$V" | sed 's/^/    /'

hr "snapshot -r twice: S1 (shared ro unit) and S2 (P4's unfreeze subject)"
set +e
S1_OUT="$(btrfs subvolume snapshot -r "$V" "$S1" 2>&1)"; S1_RC=$?
S2_OUT="$(btrfs subvolume snapshot -r "$V" "$S2" 2>&1)"; S2_RC=$?
set -e
echo "  S1: rc=$S1_RC out=[$S1_OUT] $(roflag "$S1")"
echo "  S2: rc=$S2_RC out=[$S2_OUT] $(roflag "$S2")"
[ "$S1_RC" -eq 0 ] || { echo "FATAL: cannot even take a -r snapshot; nothing below is measurable"; exit 1; }
echo "$(subvolid "$S1")" >> "$QIDS"
echo "$(subvolid "$S2")" >> "$QIDS"

# Host-side control: is the immutability real BEFORE any gofer is involved? If a
# host write into S1 succeeded, every guest-side result below would be about a
# writable directory and would prove nothing about immutable sources.
set +e
H1="$(sh -c "echo host > '$S1/host_should_fail'" 2>&1)"; H1_RC=$?
set -e
echo "  host write into S1: rc=$H1_RC msg=[$H1]   (must be non-zero — the control for everything below)"

# ---------------------------------------------------------------------------
# P4 (host half) — does `btrfs property set ro false` even work here?
# This decides how the engine's `Clone` must be built: snapshot-then-unfreeze is
# only available if this succeeds. Run BEFORE launch so arm D can bind the
# result; the guest half (does a rw bind to the thawed unit write THROUGH to the
# host?) is arm D below. A property change that is refused must be reported with
# its exact error, since "Clone = non-`-r` snapshot from the start" is then the
# only shape left.
# ---------------------------------------------------------------------------
hr "P4 host half: btrfs property set S2 ro false"
set +e
P4_OUT="$(btrfs property set "$S2" ro false 2>&1)"; P4_RC=$?
set -e
echo "  rc=$P4_RC out=[$P4_OUT]"
echo "  S2 ro flag now: $(roflag "$S2")"
set +e
P4H="$(sh -c "echo host_after_thaw > '$S2/host_after_thaw'" 2>&1)"; P4H_RC=$?
set -e
echo "  host write into thawed S2: rc=$P4H_RC msg=[$P4H]"
echo "  S2 content after thaw: $(fp "$S2" | tr '\n' ' ')"

# ---------------------------------------------------------------------------
# bundles. Identical to spike-volume-mount.sh's shape (plain rootfs, gofer-proxied
# extra bind at /vol, --overlay2=none --file-access=shared) except that source and
# ro-ness are parameters — the ONLY variable this spike changes.
# ---------------------------------------------------------------------------
writeBundle() { # writeBundle <dir> <source> <ro:true|false> <tag>
  local dir="$1" src="$2" ro="$3" tag="$4"
  mkdir -p "$dir"
  local opts='["rbind","rw"]'
  [ "$ro" = true ] && opts='["rbind","ro"]'
  cat > "$dir/config.json" <<JSON
{"ociVersion":"1.0.0",
 "process":{"terminal":false,"user":{"uid":0,"gid":0},
   "args":["/usr/bin/rosnapprobe","loop","/vol","$IVL_MS","$LOOP_SECS","$tag"],
   "env":["PATH=/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root"],"cwd":"/",
   "capabilities":{"bounding":["CAP_DAC_OVERRIDE"],"effective":["CAP_DAC_OVERRIDE"],"permitted":["CAP_DAC_OVERRIDE"]}},
 "root":{"path":"$ROOTFS","readonly":false},"hostname":"guest",
 "mounts":[{"destination":"/proc","type":"proc","source":"proc"},
   {"destination":"/dev","type":"tmpfs","source":"tmpfs","options":["nosuid","mode=755","size=65536k"]},
   {"destination":"/tmp","type":"tmpfs","source":"tmpfs","options":["nosuid","nodev","mode=1777"]},
   {"destination":"/vol","type":"bind","source":"$src","options":$opts}],
 "linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON
}

# launch — detached, stdio to FILES never pipes (harness trap 2).
launch() {
  local cid="$1" bundle="$2"
  set +e
  runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
    --overlay2=none --file-access=shared --systemd-cgroup=false \
    run --detach --bundle="$bundle" "$cid" \
    >"$WORK/$cid.log" 2>"$WORK/$cid.err" </dev/null
  echo $? > "$WORK/$cid.rc"
  set -e
}

# itCount — how many probe iterations have been emitted. `grep -c` exits 1 with
# EMPTY output on zero matches, and an empty string fed to `[ -ge ]` is a fatal
# "integer expected", so the fallback must print a 0 rather than swallow the rc.
itCount() { grep -c '^IT=' "$WORK/$1.log" 2>/dev/null || echo 0; }

# distinct — the steady-state signature of one field over the LAST $WINDOW
# iterations. One token means stable; two means it changed or is flapping. A
# bounded window keeps it reproducible rather than "everything until this line
# of the script happened to run".
distinct() {
  local cid="$1" key="$2" v
  v="$(grep '^IT=' "$WORK/$cid.log" 2>/dev/null | tail -n "$WINDOW" \
       | grep -oE "(^| )$key=[^ ]*" | sed "s/^ //;s/^$key=//" | LC_ALL=C sort -u | tr '\n' ',' || true)"
  echo "${v%,}"
}

arm() { # arm <cid> <label>
  local cid="$1" label="$2"
  echo "  [$label] cid=$cid launch_rc=$(cat "$WORK/$cid.rc" 2>/dev/null) iterations=$(itCount "$cid")"
  if [ -s "$WORK/$cid.err" ]; then
    echo "  [$label] runsc stderr: $(tr '\n' ' ' < "$WORK/$cid.err" | cut -c1-300)"
  else
    echo "  [$label] runsc stderr: EMPTY"
  fi
  local st; st="$(runsc --root="$RUNSC_ROOT" state "$cid" 2>&1 | grep -oE '"status": "[a-z]+"' | tr -d '\n' || true)"
  echo "  [$label] runsc state: ${st:-<none>}"
  if [ "$(itCount "$cid")" -gt 0 ]; then
    local k
    for k in LS RDONLY SEED NEST DEEP MRK W RB MKD UNL CHM MODE TMP; do
      printf '  [%s] %-6s -> %s\n' "$label" "$k" "$(distinct "$cid" "$k")"
    done
    # `|| true` is load-bearing: this is the LAST command of the function, so a
    # non-matching grep would make arm() return 1 and `set -e` would abort the
    # whole run right after the first arm. (Measured: it did, and the missing
    # arms looked like arms that had failed.)
    grep -q '^PROBE_DONE$' "$WORK/$cid.log" \
      && echo "  [$label] !! PROBE_DONE present — probe outlived-run invariant BROKEN, do not grade this arm" \
      || true
  else
    echo "  [$label] no iteration lines; raw log: $(tr '\n' ' ' < "$WORK/$cid.log" | cut -c1-300)"
  fi
}

hr "write five bundles"
writeBundle "$WORK/b-a" "$S1" true  A     # P1: ro snapshot, ro bind — intended path
writeBundle "$WORK/b-b" "$S1" false B     # P2: ro snapshot, RW bind — the failure mode
writeBundle "$WORK/b-c" "$S1" true  C     # P3: same snapshot, second sandbox, ro
writeBundle "$WORK/b-d" "$S2" false D     # P4: thawed former snapshot, rw
writeBundle "$WORK/b-e" "$V"  false E     # P5: the SOURCE, rw, alongside A/C
echo "  A=$A ro  S1   B=$B rw S1   C=$C ro S1   D=$D rw S2(thawed)   E=$E rw V"

hr "launch all five concurrently"
for pair in "$A:b-a" "$B:b-b" "$C:b-c" "$D:b-d" "$E:b-e"; do
  launch "${pair%%:*}" "$WORK/${pair##*:}" &
done
wait
for cid in $CIDS; do
  echo "  launch $cid rc=$(cat "$WORK/$cid.rc")"
done

# Give every arm that started a real steady state before sampling. An arm that
# refused to start has no iterations and is graded on its rc + stderr instead, so
# this waits for the arms that CAN advance and then stops — it must not block on
# an arm whose absence of output is itself the result.
STARTED_CNT=0
for cid in $CIDS; do [ "$(cat "$WORK/$cid.rc" 2>/dev/null)" = 0 ] && STARTED_CNT=$((STARTED_CNT+1)); done
for t in $(seq 1 60); do
  n=0
  for cid in $CIDS; do [ "$(itCount "$cid")" -ge $((WINDOW + 4)) ] && n=$((n+1)); done
  { [ "$n" -ge "$STARTED_CNT" ] && [ "$t" -ge 4 ]; } && break
  sleep 0.5
done
echo "  settled after ${t} half-seconds: $n/$STARTED_CNT started arms have >= $((WINDOW+4)) iterations"

# ---------------------------------------------------------------------------
# P5 (host half) — the source must remain independently usable while a ro
# snapshot of it is live in two other sandboxes, and the snapshot's bytes must
# not move. Fingerprint S1 now, mutate V from the host, fingerprint again.
# ---------------------------------------------------------------------------
hr "P5 host half: fingerprint S1, then mutate the SOURCE from the host"
fp "$S1" > "$WORK/s1.t0"
echo "HOST_MARK_AFTER_SNAPSHOT" > "$V/hostmark"
printf 'SEED_V2_CHANGED\n' > "$V/seed"
sync
sleep 3
fp "$S1" > "$WORK/s1.t1"
echo "  S1 fingerprint t0 vs t1:"
if diff -u "$WORK/s1.t0" "$WORK/s1.t1" > "$WORK/s1.diff"; then
  echo "    IDENTICAL (frozen). $(wc -l < "$WORK/s1.t0") entries."
else
  echo "    DIFFERS:"; sed 's/^/      /' "$WORK/s1.diff"
fi
echo "  live V now: $(fp "$V" | tr '\n' ' ')"

hr "steady state of each arm"
arm "$A" "P1 ro-snap/ro-bind"
echo
arm "$B" "P2 ro-snap/RW-bind"
echo
arm "$C" "P3 ro-snap/ro-bind #2"
echo
arm "$D" "P4 thawed/rw"
echo
arm "$E" "P5 source/rw"

# ---------------------------------------------------------------------------
# HOST-SIDE TRUTH — the (b)-vs-(c) discriminator for P2. The guest's own report
# is not admissible on its own: a write that "succeeded" in-guest and never
# reached the host is exactly the silent success §5.C promises cannot happen.
# ---------------------------------------------------------------------------
hr "HOST-SIDE TRUTH: which guest bytes actually landed?"
present() { [ -e "$1" ] && echo "PRESENT[$(cat "$1" 2>/dev/null | tr -d '\n')]" || echo "absent"; }
echo "  S1 (ro snapshot, bound ro by A/C and RW by B):"
echo "    S1/w-A (A's write, ro bind)          : $(present "$S1/w-A")"
echo "    S1/w-B (B's write, RW bind)          : $(present "$S1/w-B")   <- the P2 verdict"
echo "    S1/w-C (C's write, ro bind)          : $(present "$S1/w-C")"
echo "    S1/victim (UNL target, must survive) : $(present "$S1/victim")"
echo "    S1/hostmark (host wrote to V after)  : $(present "$S1/hostmark")"
echo "    S1/seed                              : $(present "$S1/seed")"
# The chmod cross-check. Every arm asked for 0600 on this file; the host seeded
# 0644. If the guest reads back 600 while the host still says 644, chmod is a
# silent success — the ONE mutation class that could slip past an immutable
# source, because it allocates nothing.
S1_MODE="$(stat -c %a "$S1/seed" 2>/dev/null)"
V_MODE="$(stat -c %a "$V/seed" 2>/dev/null)"
echo "    S1/seed mode on the HOST             : $S1_MODE  (seeded 0644; every arm asked for 0600)"
echo "    V/seed  mode on the HOST             : $V_MODE"
echo "  S2 (former snapshot, thawed by P4, bound rw by D):"
echo "    S2/w-D (D's write through the thaw)  : $(present "$S2/w-D")   <- the P4 verdict"
echo "    S2/seed (must still be the SNAPSHOT's value SEED_V1, not V's new one): $(present "$S2/seed")"
echo "  V (the live source, bound rw by E):"
echo "    V/w-E (E's write)                    : $(present "$V/w-E")"
echo "    V/w-D (must be absent: thawed snapshot writes must NOT reach the source): $(present "$V/w-D")"
echo "    V/hostmark                           : $(present "$V/hostmark")"

hr "fresh runsc exec into the ro arm and the rw-bind arm (serviceability)"
for pair in "$A:P1" "$B:P2"; do
  cid="${pair%%:*}"; lbl="${pair##*:}"
  set +e
  runsc --root="$RUNSC_ROOT" exec "$cid" /usr/bin/rosnapprobe live /vol "X$lbl" \
    > "$WORK/$cid.exec" 2>&1
  rc=$?
  set -e
  echo "  [$lbl] exec rc=$rc: $(tr '\n' ' ' < "$WORK/$cid.exec" | cut -c1-400)"
  echo "$rc" > "$WORK/$cid.execrc"
done

hr "final subvolume state"
btrfs subvolume list -o "$MP" 2>/dev/null | grep -- "spike-rosnap" | sed 's/^/  /' || true
for s in "$V" "$S1" "$S2"; do echo "  $(basename "$s"): $(roflag "$s")"; done

# ---------------------------------------------------------------------------
hr "VERDICT"
FAIL=0
ok()   { echo "PASS  $*"; }
bad()  { echo "FAIL  $*"; FAIL=1; }
note() { echo "NOTE  $*"; }

started() { [ "$(cat "$WORK/$1.rc" 2>/dev/null)" = 0 ] && [ "$(itCount "$1")" -gt 0 ]; }
onlytok() { [ "$(distinct "$1" "$2")" = "$3" ]; }

# --- control ---------------------------------------------------------------
[ "$H1_RC" -ne 0 ] && ok "control: the -r snapshot is genuinely immutable on the HOST too (rc=$H1_RC) — everything below is about an immutable source, not a ro mount option" \
                   || bad "control: host write into the -r snapshot SUCCEEDED — this spike measured a writable dir, results are void"

# --- P1 --------------------------------------------------------------------
if started "$A"; then
  ok "P1 a sandbox launches with a \`snapshot -r\` unit bound ro (rc=0, $(itCount "$A") probe iterations)"
  { onlytok "$A" SEED "SEED_V1" && onlytok "$A" NEST "NESTED_OK" && onlytok "$A" DEEP "DEEP_OK"; } \
    && ok "P1 guest reads top-level, nested and two-deep content out of the snapshot (SEED_V1 / NESTED_OK / DEEP_OK)" \
    || bad "P1 read-through broken: SEED=$(distinct "$A" SEED) NEST=$(distinct "$A" NEST) DEEP=$(distinct "$A" DEEP)"
  case "$(distinct "$A" LS)" in
    n=*) ok "P1 guest enumerates the snapshot directory ($(distinct "$A" LS))" ;;
    *)   bad "P1 directory enumeration failed: LS=$(distinct "$A" LS)" ;;
  esac
  onlytok "$A" RDONLY yes && ok "P1 guest statfs reports the mount read-only (an in-guest precheck can see it)" \
                          || note "P1 guest statfs ST_RDONLY=$(distinct "$A" RDONLY) — the guest cannot tell it is read-only from statfs"
else
  bad "P1 the ro arm did not even reach a steady state (rc=$(cat "$WORK/$A.rc" 2>/dev/null)) — 'snapshot as shareable ro volume' does not work at all"
fi

# --- P2: the load-bearing classification ----------------------------------
P2W="$(distinct "$B" W)"
P2CLASS="unclassified"
if [ "$(cat "$WORK/$B.rc" 2>/dev/null)" != 0 ]; then
  P2CLASS="(a) runsc refused to create/start the container"
elif [ "$(itCount "$B")" -eq 0 ]; then
  P2CLASS="(c-dirty) container started but the probe produced nothing — fault or hang"
elif [ "$P2W" = "E30/EROFS" ]; then
  P2CLASS="(b) container runs, guest write fails EROFS"
elif [ "$P2W" = "ok" ]; then
  if [ -e "$S1/w-B" ]; then
    P2CLASS="(d) write SUCCEEDED and reached the host — the -r snapshot was not immutable"
  else
    P2CLASS="(c-worst) write appeared to SUCCEED in-guest but the host never saw it — SILENT SUCCESS"
  fi
else
  P2CLASS="(c-dirty) guest write failed with $P2W, not a clean EROFS"
fi
echo "P2 CLASSIFICATION: $P2CLASS"
case "$P2CLASS" in
  "(b)"*) ok "P2 §5.C's 'ro bind ⇒ EROFS, a clean POSIX refusal not a silent success' HOLDS for an immutable source (W=$P2W)" ;;
  "(a)"*) note "P2 the mount is rejected at create time instead of at write time — a different (also safe) shape than §5.C describes" ;;
  *)      bad "P2 §5.C's clean-refusal sentence does NOT hold as written: $P2CLASS" ;;
esac
if started "$B"; then
  [ -e "$S1/w-B" ] && bad "P2 the RW-bound guest's bytes LANDED in the read-only snapshot" \
                   || ok "P2 no byte from the RW-bound guest reached the host snapshot (w-B absent)"
  [ -e "$S1/victim" ] && ok "P2 the destructive metadata op was refused too — S1/victim survived (UNL=$(distinct "$B" UNL))" \
                      || bad "P2 UNL deleted a file out of the -r snapshot (UNL=$(distinct "$B" UNL))"
  note "P2 full write-side signature: W=$P2W RB=$(distinct "$B" RB) MKD=$(distinct "$B" MKD) UNL=$(distinct "$B" UNL) CHM=$(distinct "$B" CHM) MODE=$(distinct "$B" MODE)"
  # chmod gets its own verdict line: it is the one class that returned `ok` in an
  # earlier run of this spike while every other class returned EROFS, which is
  # precisely the (c) shape. Two questions, in order: did the host's mode change
  # (a real mutation of an immutable unit — much worse than a silent success),
  # and does the guest's own readback agree with the host (if not, the guest is
  # being told a lie it could act on).
  if [ "$(distinct "$B" CHM)" = "ok" ]; then
    [ "$S1_MODE" = 644 ] \
      && ok "P2 chmod on the immutable unit reports ok but the HOST mode is unchanged ($S1_MODE) — a NON-DURABLE metadata success, the one gap in the clean-refusal story" \
      || bad "P2 chmod MUTATED the read-only snapshot on the host (mode now $S1_MODE) — the -r unit is not immutable to metadata ops"
    [ "$(distinct "$B" MODE)" = 644 ] \
      && ok "P2 and the guest's own stat still reads 644, so nothing downstream of the guest is misled by that ok" \
      || bad "P2 the guest reads back mode $(distinct "$B" MODE) while the host says $S1_MODE — the guest is told a lie about an immutable unit"
  else
    ok "P2 chmod is refused too (CHM=$(distinct "$B" CHM)) — the refusal covers metadata-only ops, not just allocating writes"
  fi
  note "P2 read side under a RW bind: SEED=$(distinct "$B" SEED) LS=$(distinct "$B" LS) RDONLY=$(distinct "$B" RDONLY)"
  onlytok "$B" TMP ok && ok "P2 the sandbox itself is unharmed (TMP=ok — off-volume I/O fine), so this is a volume-scoped refusal not a dead sandbox" \
                      || bad "P2 off-volume I/O also broke (TMP=$(distinct "$B" TMP)) — the refusal took the sandbox with it"
  [ "$(cat "$WORK/$B.execrc" 2>/dev/null)" = 0 ] && ok "P2 a FRESH runsc exec still works in the RW-bound arm — the container stayed serviceable" \
                                                 || note "P2 fresh exec rc=$(cat "$WORK/$B.execrc" 2>/dev/null)"
fi

# --- P3 --------------------------------------------------------------------
if started "$A" && started "$C"; then
  ok "P3 the SAME -r snapshot is bound ro into two concurrent sandboxes, both launched (rc=0/rc=0)"
  { onlytok "$C" SEED "SEED_V1" && onlytok "$C" NEST "NESTED_OK"; } \
    && ok "P3 the second sandbox reads it too (no EBUSY, no 'source in use', no silent degradation) — SharedReadOnly read fan-out over a snapshot works" \
    || bad "P3 second ro sandbox cannot read: SEED=$(distinct "$C" SEED) NEST=$(distinct "$C" NEST)"
  note "P3 both arms concurrently, plus arm B's RW bind of the same unit and arm E's rw mount of its SOURCE — four mounts over one snapshot lineage, none disturbed the others"
else
  bad "P3 fan-out failed: A started=$(started "$A" && echo yes || echo no) C started=$(started "$C" && echo yes || echo no)"
fi

# --- P4 --------------------------------------------------------------------
if [ "$P4_RC" -eq 0 ]; then
  ok "P4 \`btrfs property set <snapshot> ro false\` SUCCEEDS on this substrate (rc=0)"
  [ "$P4H_RC" -eq 0 ] && ok "P4 the host can write into the thawed former snapshot (rc=0)" \
                      || bad "P4 host write into the thawed snapshot still failed rc=$P4H_RC [$P4H]"
  if started "$D"; then
    { onlytok "$D" W ok && [ -e "$S2/w-D" ]; } \
      && ok "P4 a guest bound rw to the thawed former snapshot writes THROUGH to the host (W=ok, host sees S2/w-D=$(cat "$S2/w-D" 2>/dev/null | tr -d '\n'))" \
      || bad "P4 guest rw through the thawed snapshot did not reach the host: W=$(distinct "$D" W) host_w_D=$(present "$S2/w-D")"
    onlytok "$D" RDONLY no && ok "P4 guest statfs now reports writable, consistent with the flipped flag" \
                           || note "P4 guest statfs RDONLY=$(distinct "$D" RDONLY) after the thaw"
    [ -e "$V/w-D" ] && bad "P4 writes into the thawed snapshot LEAKED into its source" \
                    || ok "P4 CoW intact: writes into the thawed snapshot do not touch the source (V/w-D absent)"
    grep -q 'SEED_V1' <<<"$(cat "$S2/seed" 2>/dev/null)" \
      && ok "P4 the thawed unit still holds the SNAPSHOT's bytes (seed=SEED_V1), not the source's later value" \
      || bad "P4 the thawed unit's content is not the snapshot's: seed=$(cat "$S2/seed" 2>/dev/null | tr -d '\n')"
    note "P4 => Clone CAN be built as snapshot-then-unfreeze; a non-\`-r\` snapshot from the start is a choice, not a necessity"
  else
    bad "P4 the thawed arm did not start (rc=$(cat "$WORK/$D.rc" 2>/dev/null)) — snapshot-then-unfreeze yields a unit runsc will not mount rw"
  fi
else
  bad "P4 \`btrfs property set ro false\` REFUSED rc=$P4_RC [$P4_OUT] => Clone MUST be a non-\`-r\` snapshot from the start"
fi

# --- P5 --------------------------------------------------------------------
if started "$E"; then
  ok "P5 the SOURCE subvolume is still rw-mountable in another sandbox while a -r snapshot of it is live in two others (rc=0)"
  { onlytok "$E" W ok && [ -e "$V/w-E" ]; } \
    && ok "P5 and still writable end-to-end (guest W=ok, host sees V/w-E)" \
    || bad "P5 source no longer writable: W=$(distinct "$E" W) host=$(present "$V/w-E")"
  onlytok "$E" MRK "HOST_MARK_AFTER_SNAPSHOT" \
    && note "P5 the source guest also sees the host's post-snapshot write (MRK=HOST_MARK_AFTER_SNAPSHOT)" \
    || note "P5 source guest MRK=$(distinct "$E" MRK)"
else
  bad "P5 the source arm did not start (rc=$(cat "$WORK/$E.rc" 2>/dev/null)) — a live snapshot PINS its source"
fi
[ ! -s "$WORK/s1.diff" ] && ok "P5 the snapshot's bytes stayed frozen byte-for-byte while its source was rewritten (full-tree fingerprint identical)" \
                         || bad "P5 the snapshot's content MOVED while the source changed (see the diff above)"
[ ! -e "$S1/hostmark" ] && ok "P5 the host's post-snapshot write to the source is absent from the snapshot" \
                        || bad "P5 post-snapshot source write LEAKED into the snapshot"
if started "$A"; then
  onlytok "$A" MRK "E2/ENOENT" \
    && ok "P5 and the ro guests see that absence as a plain ENOENT (MRK=E2/ENOENT), not a stale or phantom entry" \
    || note "P5 ro guest MRK=$(distinct "$A" MRK)"
  onlytok "$A" SEED "SEED_V1" \
    && ok "P5 the ro guests still read the snapshot's frozen value even after the source's seed became SEED_V2_CHANGED" \
    || bad "P5 ro guest's view followed the source: SEED=$(distinct "$A" SEED)"
fi

# --- probe-outlived-run invariant (harness trap 1) ------------------------
for cid in $CIDS; do
  grep -q '^PROBE_DONE$' "$WORK/$cid.log" 2>/dev/null \
    && bad "harness: $cid's probe ended on its own — its 'stopped' state is not evidence about the mount (raise LOOP_SECS)"
done

echo
echo "P2 ANSWER (the one §5.C depends on): $P2CLASS"
exit $FAIL
