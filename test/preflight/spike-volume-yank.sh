#!/usr/bin/env bash
# spike-volume-yank.sh — BLAST RADIUS of a mis-sweep: what happens when a
# Volume's backing bytes are removed from under a LIVE guest?
#
# WHY THIS MEASUREMENT EXISTS. the documented invariant's mount table protects a mounted Volume with a
# bidirectional FK RESTRICT: a volume with a live mount row cannot be deleted.
# But `SweepOrphanVolumes` (the documented invariant) is a DIFFERENT path — it reasons from
# ENUMERATION of backend units, not from the FK — and the documented invariant's sweep safety rule
# exists because the obvious enumeration primitive is a loaded gun:
# `btrfs subvolume list <dir>` returns the WHOLE FILESYSTEM (37 rows, including
# docker, k8s and 9 OrbStack container subvolumes) no matter which path you hand
# it. the documentation the documented invariant makes reclamation protect-on-uncertainty precisely because
# it is irreversible. So the open question is not "can a sweep be wrong" — it is
# HOW BAD a wrong sweep is:
#
#   * if the sandbox DIES, a mis-sweep is a LIFECYCLE event: the documented invariant crash
#     semantics catch it, the control plane sees a state transition, an operator
#     sees an event.
#   * if the guest merely takes I/O errors, it is a DATA-PLANE event: nothing
#     reaches the control plane at all (same shape as the measured ENOSPC in
#     deep-dives/the relevant internals page the documented invariant), and a silent mis-sweep can destroy a customer's
#     bytes while every engine-side signal stays green.
#
# That difference decides how paranoid the sweep must be and what the M7
# acceptance test has to assert. Measure it; do not reason about it.
#
# WHY EACH PROBE EXISTS
#   loop probe (all arms)  — the guest must be ACTIVELY using the volume across
#     the mutation, and must NOT exit on first error: a first fault tells you
#     nothing about steady state (retry could recover, or hang, or change errno).
#     Each iteration exercises six independent paths, because they can and do
#     diverge: fresh create+write+fsync (W), read-back (R), creating a NEW
#     directory entry (NW — openat O_CREAT in a deleted directory is the classic
#     ENOENT case), write/read through an fd OPENED BEFORE the mutation (HW/HR —
#     this is what tells us whether the gofer keeps serving stale bytes against
#     an unlinked inode), readdir (LS), stat of the mount root (ST), and a
#     tmpfs write/read (TMP) that is NOT on the volume and therefore reports on
#     the SANDBOX rather than the volume.
#   host side, per arm — `runsc state` + a FRESH `runsc exec`. In-guest liveness
#     could in principle be a limping sentry; a new process proves the container
#     is genuinely serviceable, and `state` + stderr prove whether anything at
#     all became visible to the control plane.
#   five mutation arms — a mis-sweep is not one action. `subvolume delete`,
#     `rm -rf` of contents, one unlinked file, a qgroup row destroyed without
#     the subvolume, and a `mv` aside are all plausible sweep bugs; if their
#     blast radii differ, THAT SPREAD is the finding.
#   relaunch arms — the recovery question. If `runsc run` refuses to start when
#     a bind source is absent, a mis-swept sandbox is permanently unlaunchable;
#     if it silently creates an empty dir, the loss is data-only. That single
#     bit decides "recoverable with data loss" vs "permanently broken".
#
# ISOLATION: private --root, unique CID prefix, --network=none, no netns, own
# subvolumes under /var/tmp — other agents run runsc on this machine concurrently.
#
# Usage: sudo ./spike-volume-yank.sh   (needs root: btrfs + runsc gofer)
set -euo pipefail

LOOP_SECS=900        # MUST outlive every arm's observation window. If the probe
                     # exits on its own, PID 1 exits and the container goes
                     # `stopped` — which would look exactly like "the sandbox
                     # died from the mutation". Generous budget + cleanup kills
                     # them; classify() also refuses to grade an arm whose log
                     # contains PROBE_DONE.
IVL_MS=200           # 5 iterations/sec: fine-grained enough to catch a hang
CAP_BYTE=$((8 * 1024 * 1024))   # arm (c): small cap, fast to fill

WORK="$(mktemp -d /tmp/spike-vol-yank.XXXXXX)"
ROOTFS="$WORK/rootfs"; RUNSC_ROOT="$WORK/runsc-root"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
TAG="spike-yank-$$"
# Volumes MUST be on real btrfs (/var/tmp) — subvolume/qgroup mutations are the
# whole point. Bundles/rootfs live on /tmp (native fs, the recorded design).
V1="/var/tmp/$TAG-1"   # arm 1: btrfs subvolume delete
V2="/var/tmp/$TAG-2"   # arm 5a: rm -rf contents, subvolume survives
V3="/var/tmp/$TAG-3"   # arm 5b: unlink one file the guest holds open
V4="/var/tmp/$TAG-4"   # arm 5c: qgroup row only (capped)
V5="/var/tmp/$TAG-5"   # arm 5d: mv the subvolume aside
V5M="/var/tmp/$TAG-5-moved"
VOLS="$V1 $V2 $V3 $V4 $V5 $V5M"
A="s13y$$a"; B="s13y$$b"; C="s13y$$c"; D="s13y$$d"; E="s13y$$e"
F="s13y$$f"; G="s13y$$g"; H="s13y$$h"
CIDS="$A $B $C $D $E $F $G $H"
QIDS="$WORK/qids"; : > "$QIDS"

cleanup() {
  set +e
  for cid in $CIDS; do
    sudo runsc --root="$RUNSC_ROOT" kill "$cid" KILL 2>/dev/null
    sudo runsc --root="$RUNSC_ROOT" delete --force "$cid" 2>/dev/null
  done
  for v in $VOLS; do
    sudo btrfs qgroup limit none "$v" 2>/dev/null
    sudo btrfs subvolume delete "$v" 2>/dev/null
  done
  # squota leaks a qgroup row per storage unit that outlived a snapshot or a
  # racing delete (deep-dives/the relevant internals page the documented invariant) — sweep OUR ids explicitly.
  sync; sudo btrfs subvolume sync /var/tmp >/dev/null 2>&1
  while read -r qid; do
    [ -n "$qid" ] && sudo btrfs qgroup destroy "0/$qid" /var/tmp 2>/dev/null
  done < "$QIDS"
  echo "-- cleanup residue (must be empty) --"
  sudo btrfs subvolume list /var/tmp 2>/dev/null | grep -- "$TAG" || echo "  no subvolumes left"
  sudo btrfs qgroup show -re --raw --sync -f /var/tmp 2>/dev/null \
    | awk -v ids="$(tr '\n' ' ' < "$QIDS")" 'NR>2 { split(ids,a," "); for(i in a) if ($1=="0/"a[i]) print "  LEFTOVER "$0 }'
  sudo rm -rf "$WORK"
  set -e
}
trap cleanup EXIT

# --- the probe -----------------------------------------------------------------
# Static (CGO off) so the bare rootfs needs no loader/libc. Prints machine-
# greppable KEY=VALUE lines with RAW errno integers — dash builtins could only
# say "it failed", and the whole question here is WHICH failure.
cat > "$WORK/probe.go" <<'GO'
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

func ename(e syscall.Errno) string {
	switch e {
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EIO:
		return "EIO"
	case syscall.ESTALE:
		return "ESTALE"
	case syscall.ENOSPC:
		return "ENOSPC"
	case syscall.EROFS:
		return "EROFS"
	case syscall.EACCES:
		return "EACCES"
	case syscall.EPERM:
		return "EPERM"
	case syscall.EBADF:
		return "EBADF"
	case syscall.ENOTCONN:
		return "ENOTCONN"
	case syscall.ENODEV:
		return "ENODEV"
	case syscall.EBUSY:
		return "EBUSY"
	}
	return "OTHER"
}

// res renders one probe outcome as a single token: ok, or E<errno>/<NAME>.
// One token per path means the whole steady state fits on one line, and a
// change of behaviour shows up as a change of token — which is what the host
// side diffs before/after the mutation.
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

const payload = "PROBE_PAYLOAD"

// writeFresh: create+truncate+write+fsync+close. fsync explicitly, because a
// failure deferred to fsync/close would make "the guest sees an error" a much
// weaker claim than it looks.
func writeFresh(p string) error {
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

func readBack(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return res(err)
	}
	if string(b) != payload {
		return "BADCONTENT"
	}
	return "ok"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: yankprobe loop|fill|live ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "loop":
		loop(os.Args[2], atoi(os.Args[3]), atoi(os.Args[4]))
	case "fill":
		fill(os.Args[2], atoi(os.Args[3]), atoi(os.Args[4]), atoi(os.Args[5]))
	case "live":
		live(os.Args[2])
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
GO

# (probe part 2 — appended, same file: keeps each chunk reviewable)
cat >> "$WORK/probe.go" <<'GO'

// loop is the heart of the measurement: one line per iteration, six independent
// paths, and it NEVER exits on error. Steady state is the question — a single
// first fault cannot distinguish "permanently broken", "recovers", and "hangs".
//
// held is opened ONCE, BEFORE any host-side mutation. Writing/reading through it
// afterwards is the only way to see whether the gofer keeps serving an unlinked
// inode (stale-but-readable) or the sentry hard-fails the fd.
func loop(vol string, ivlMS, secs int) {
	fresh := vol + "/fresh"
	held := vol + "/held"
	if err := writeFresh(held); err != nil {
		fmt.Printf("SETUP_HELD_WRITE=%s\n", res(err))
	}
	hf, herr := os.OpenFile(held, os.O_RDWR, 0o644)
	fmt.Printf("PROBE_START held_open=%s pid=%d\n", res(herr), os.Getpid())
	os.Stdout.Sync()
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for it := 1; time.Now().Before(deadline); it++ {
		w := res(writeFresh(fresh))
		r := readBack(fresh)

		// NW: a NEW directory entry each iteration. openat(O_CREAT) in a
		// directory whose host inode is gone is the classic hard-failure case,
		// and it is distinct from rewriting an entry that already exists.
		nwp := fmt.Sprintf("%s/new-%d", vol, it)
		nw := res(writeFresh(nwp))
		rm := res(os.Remove(nwp))

		// HW/HR: the pre-mutation fd. pwrite/pread at offset 0, no path lookup.
		hw, hr := "noFD", "noFD"
		if hf != nil {
			_, e := hf.WriteAt([]byte(payload), 0)
			hw = res(e)
			if e == nil {
				hw = res(hf.Sync())
			}
			buf := make([]byte, len(payload))
			if _, e := hf.ReadAt(buf, 0); e != nil {
				hr = res(e)
			} else if string(buf) != payload {
				hr = "BADCONTENT"
			} else {
				hr = "ok"
			}
		}

		ls := "?"
		if ents, e := os.ReadDir(vol); e != nil {
			ls = res(e)
		} else {
			ls = fmt.Sprintf("n=%d", len(ents))
		}

		st, ino := "?", uint64(0)
		if fi, e := os.Stat(vol); e != nil {
			st = res(e)
		} else {
			st = "ok"
			if s, ok := fi.Sys().(*syscall.Stat_t); ok {
				ino = uint64(s.Ino)
			}
		}

		// SEED was written by the HOST before launch. Its content identifies
		// WHICH host directory the guest is actually talking to — the discriminator
		// for the `mv` arm (moved subvol says SEEDED, a decoy at the old path
		// says DECOY) and the stale-read question for the delete arm.
		seed := "?"
		if b, e := os.ReadFile(vol + "/seed"); e != nil {
			seed = res(e)
		} else {
			seed = string(b)
		}

		// TMP is NOT on the volume: it reports on the SANDBOX, not the volume.
		// This is the line that separates "volume I/O broke" from "sandbox died".
		tmp := res(writeFresh("/tmp/live"))
		if tmp == "ok" {
			tmp = readBack("/tmp/live")
		}

		fmt.Printf("IT=%d W=%s R=%s NW=%s RM=%s HW=%s HR=%s LS=%s ST=%s STINO=%d SEED=%s TMP=%s\n",
			it, w, r, nw, rm, hw, hr, ls, st, ino, seed, tmp)
		os.Stdout.Sync()
		time.Sleep(time.Duration(ivlMS) * time.Millisecond)
	}
	fmt.Println("PROBE_DONE")
}
GO
# (probe part 3)
cat >> "$WORK/probe.go" <<'GO'

// fill drives arm (c): append until the cap bites, then KEEP APPENDING. The
// question is whether removing the host-side qgroup ROW (subvolume intact) lifts
// the cap for a guest that is already wedged against it — i.e. whether a sweep
// that reclaims only the quota row silently converts a capped volume into an
// uncapped one. Cumulative bytes are printed so the host can compare against the
// old limit.
func fill(vol string, chunk, ivlMS, secs int) {
	p := vol + "/fill"
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Printf("FILL_OPEN=%s\n", res(err))
		return
	}
	fmt.Printf("PROBE_START fill=%s pid=%d\n", p, os.Getpid())
	os.Stdout.Sync()
	buf := make([]byte, chunk)
	for i := range buf {
		buf[i] = 'x'
	}
	total := 0
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for it := 1; time.Now().Before(deadline); it++ {
		n, e := f.Write(buf)
		total += n
		tmp := res(writeFresh("/tmp/live"))
		fmt.Printf("FILL it=%d total=%d n=%d W=%s TMP=%s\n", it, total, n, res(e), tmp)
		os.Stdout.Sync()
		time.Sleep(time.Duration(ivlMS) * time.Millisecond)
	}
	fmt.Println("PROBE_DONE")
}

// live is what a FRESH `runsc exec` runs: does a brand-new process in this
// sandbox work at all? In-guest loop liveness could be a limping sentry; a new
// process proves the container is genuinely serviceable.
func live(vol string) {
	fmt.Printf("EXEC_PID=%d\n", os.Getpid())
	var u syscall.Utsname
	fmt.Printf("EXEC_UNAME=%s\n", res(syscall.Uname(&u)))
	fmt.Printf("EXEC_TMP_WRITE=%s\n", res(writeFresh("/tmp/exec_live")))
	fmt.Printf("EXEC_TMP_READ=%s\n", readBack("/tmp/exec_live"))
	fmt.Printf("EXEC_VOL_WRITE=%s\n", res(writeFresh(vol+"/exec_fresh")))
	fmt.Printf("EXEC_VOL_READ=%s\n", readBack(vol+"/exec_fresh"))
	if ents, e := os.ReadDir(vol); e != nil {
		fmt.Printf("EXEC_VOL_LS=%s\n", res(e))
	} else {
		fmt.Printf("EXEC_VOL_LS=n=%d\n", len(ents))
	}
	if b, e := os.ReadFile(vol + "/seed"); e != nil {
		fmt.Printf("EXEC_SEED=%s\n", res(e))
	} else {
		fmt.Printf("EXEC_SEED=%s\n", string(b))
	}
	fmt.Println("PROBE_DONE")
}
GO

echo "== build minimal rootfs (dash + loader) + inject static yankprobe =="
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" /usr/bin/dash >/dev/null
CGO_ENABLED=0 GO111MODULE=off go build -o "$ROOTFS/usr/bin/yankprobe" "$WORK/probe.go"
chmod 0755 "$ROOTFS/usr/bin/yankprobe"
mkdir -p "$ROOTFS/vol"

echo "== create five btrfs subvolumes, seeded (a Volume = a subvolume, the documented invariant) =="
for v in $V1 $V2 $V3 $V4 $V5; do
  sudo btrfs subvolume create "$v" >/dev/null
  printf SEEDED | sudo tee "$v/seed" >/dev/null
  sudo chmod 0777 "$v"
  # Record the subvol id so cleanup can destroy a leaked squota row BY ID even
  # after the subvolume path is gone (deep-dives/the relevant internals page the documented invariant).
  sudo btrfs subvolume show "$v" | awk '/Subvolume ID:/{print $3}' >> "$QIDS"
done
sudo btrfs qgroup limit "$CAP_BYTE" "$V4"   # arm (c) needs a cap to remove
sync
echo "-- subvol ids under measurement --"; tr '\n' ' ' < "$QIDS"; echo
echo "-- arm (c) qgroup BEFORE --"; sudo btrfs qgroup show -re --raw --sync -f "$V4" | tail -1

# writeBundle <dir> <vol-source> <json-argv> — the only per-arm differences are
# the bind source and the probe argv. Guest argv must be a SINGLE PHYSICAL LINE
# (a newline inside process.args makes runsc reject the spec).
writeBundle() {
  local dir="$1" src="$2" args="$3"
  mkdir -p "$dir"
  cat > "$dir/config.json" <<JSON
{"ociVersion":"1.0.0",
 "process":{"terminal":false,"user":{"uid":0,"gid":0},
   "args":$args,
   "env":["PATH=/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root"],"cwd":"/",
   "capabilities":{"bounding":["CAP_DAC_OVERRIDE"],"effective":["CAP_DAC_OVERRIDE"],"permitted":["CAP_DAC_OVERRIDE"]}},
 "root":{"path":"$ROOTFS","readonly":false},"hostname":"guest",
 "mounts":[{"destination":"/proc","type":"proc","source":"proc"},
   {"destination":"/dev","type":"tmpfs","source":"tmpfs","options":["nosuid","mode=755","size=65536k"]},
   {"destination":"/tmp","type":"tmpfs","source":"tmpfs","options":["nosuid","nodev","mode=1777"]},
   {"destination":"/vol","type":"bind","source":"$src","options":["rbind","rw"]}],
 "linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON
}

# launch <cid> <bundle> — detached, stdio to a FILE (a pipe deadlocks
# `runsc --detach`; known engine finding — do not "simplify" this to a pipe).
launch() {
  local cid="$1" bundle="$2"
  set +e
  sudo runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
    --overlay2=none --file-access=shared --systemd-cgroup=false \
    run --detach --bundle="$bundle" "$cid" \
    >"$WORK/$cid.log" 2>"$WORK/$cid.err" </dev/null
  echo $? > "$WORK/$cid.rc"
  set -e
}

# launchFg <cid> <bundle> — foreground, for the RELAUNCH arms where the question
# is only "does runsc start at all"; its rc is the answer.
launchFg() {
  local cid="$1" bundle="$2"
  set +e
  sudo runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
    --overlay2=none --file-access=shared --systemd-cgroup=false \
    run --bundle="$bundle" "$cid" \
    >"$WORK/$cid.log" 2>"$WORK/$cid.err" </dev/null
  echo $? > "$WORK/$cid.rc"
  set -e
}

# itCount <cid> — how many loop iterations have been emitted so far. Used to mark
# the mutation instant in the stream AND to detect a HANG (count stops advancing
# while the process is still alive), which is a distinct outcome from an errno.
itCount() { grep -c '^IT=\|^FILL ' "$WORK/$1.log" 2>/dev/null || true; }

# waitAdvance <cid> <n> — wait until n more iterations appear; never fails, a
# stall is itself a finding and is reported by the caller.
waitAdvance() {
  local cid="$1" want="$2" base; base="$(itCount "$cid")"
  for _ in $(seq 1 60); do
    [ "$(itCount "$cid")" -ge $((base + want)) ] && return 0
    sleep 0.5
  done
  return 0
}

# sig <cid> <fromLine> <label> — the STEADY STATE after the mutation: for each
# probe path, the DISTINCT values it took over the post-mutation window. One
# token means stable; two means it changed or is flapping. The window is CAPPED
# at WINDOW lines so it is a bounded, reproducible sample rather than "everything
# until this line of the script happened to run". Field patterns require a
# leading space so W does not match HW/NW and ST does not match STINO.
WINDOW=40
sig() {
  local cid="$1" from="$2" label="$3" k vals
  awk -v n="$from" -v w="$WINDOW" 'NR>n && NR<=n+w && (/^IT=/ || /^FILL /)' \
    "$WORK/$cid.log" > "$WORK/$cid.$label"
  local n; n="$(wc -l < "$WORK/$cid.$label")"
  echo "  [$label] iterations_in_window=$n (window capped at $WINDOW)"
  for k in W R NW RM HW HR LS ST SEED TMP; do
    # `|| true` is load-bearing: a field absent from this arm's probe (the fill
    # arm has no R/LS) makes grep exit 1, and under `set -e` a failed command
    # substitution inside an assignment aborts the whole script.
    vals="$(grep -oE "(^| )$k=[^ ]*" "$WORK/$cid.$label" 2>/dev/null | sed "s/^ //;s/^$k=//" | sort -u | tr '\n' ',' || true)"
    [ -n "$vals" ] && echo "  [$label] $k -> ${vals%,}"
  done
  return 0
}

# hostCheck <cid> — THE LIFECYCLE-VS-DATA-PLANE DISCRIMINATOR, from outside the
# guest: container state, runsc's own stderr (would the control plane have seen
# anything?), and a FRESH `runsc exec` (rc=0 ⇒ the sandbox is not merely alive
# but serviceable). Results are stashed in files so the VERDICT can grep them.
hostCheck() {
  local cid="$1"
  local st; st="$(sudo runsc --root="$RUNSC_ROOT" state "$cid" 2>&1 | tr -d '\n' || true)"
  echo "  runsc state: $st" | cut -c1-200
  echo "$st" > "$WORK/$cid.state"
  set +e
  sudo runsc --root="$RUNSC_ROOT" exec "$cid" /usr/bin/yankprobe live /vol \
    > "$WORK/$cid.exec" 2>&1
  echo $? > "$WORK/$cid.execrc"
  set -e
  echo "  fresh runsc exec rc=$(cat "$WORK/$cid.execrc"): $(tr '\n' ' ' < "$WORK/$cid.exec")"
  if [ -s "$WORK/$cid.err" ]; then
    echo "  runsc stderr (NON-EMPTY — control plane would see this):"
    sed 's/^/    /' "$WORK/$cid.err"
  else
    echo "  runsc stderr: EMPTY (nothing reached the control plane)"
  fi
}

# ---- launch all five live arms concurrently ------------------------------------
LOOP_ARGS="[\"/usr/bin/yankprobe\",\"loop\",\"/vol\",\"$IVL_MS\",\"$LOOP_SECS\"]"
FILL_ARGS="[\"/usr/bin/yankprobe\",\"fill\",\"/vol\",\"1048576\",\"$IVL_MS\",\"$LOOP_SECS\"]"
writeBundle "$WORK/b-a" "$V1" "$LOOP_ARGS"
writeBundle "$WORK/b-b" "$V2" "$LOOP_ARGS"
writeBundle "$WORK/b-c" "$V3" "$LOOP_ARGS"
writeBundle "$WORK/b-d" "$V4" "$FILL_ARGS"
writeBundle "$WORK/b-e" "$V5" "$LOOP_ARGS"

echo
echo "== launch five live guests, each actively using its own volume =="
for pair in "$A:b-a" "$B:b-b" "$C:b-c" "$D:b-d" "$E:b-e"; do
  launch "${pair%%:*}" "$WORK/${pair##*:}" &
done
wait
for cid in $A $B $C $D $E; do
  echo "launch $cid rc=$(cat "$WORK/$cid.rc")$([ -s "$WORK/$cid.err" ] && echo "  stderr: $(tr -d '\n' < "$WORK/$cid.err")")"
done
# Let every probe get several clean iterations in first: the BEFORE baseline has
# to prove the volume was working, or "it broke" means nothing.
for cid in $A $B $C $E; do waitAdvance "$cid" 5; done
echo "-- baseline (pre-mutation) steady state, arm 1 --"
sig "$A" 0 pre

# ============================ ARM 1: subvolume delete ==========================
# THE headline case: exactly what a mis-scoped `SweepOrphanVolumes` would do to a
# live volume. Two sub-questions the host must answer before we even look at the
# guest: does btrfs let the delete through while a gofer holds files open, and is
# the path gone immediately or deferred?
echo
echo "=============== ARM 1: btrfs subvolume delete UNDER A LIVE GUEST ==============="
MARK_A="$(itCount "$A")"
V1ID="$(head -1 "$QIDS")"
set +e
DEL_OUT="$(sudo btrfs subvolume delete "$V1" 2>&1)"; DEL_RC=$?
set -e
echo "btrfs subvolume delete rc=$DEL_RC: $DEL_OUT"
echo "host: path exists right after delete? $([ -e "$V1" ] && echo YES || echo no)"
echo "host: still in subvolume list? $(sudo btrfs subvolume list /var/tmp | grep -c -- "$TAG-1$")"
# Is the DELETE ITSELF deferred while a gofer holds files open? `subvolume sync`
# waits for pending deletions to settle, so rc=124 means "still pending" — a
# distinct fact from "the directory entry is gone". Bounded with `timeout`
# because with a live gofer this genuinely blocks for MINUTES (measured: it did,
# on the first run of this spike, and silently stretched every later arm's
# window past the probe's own deadline).
set +e
timeout 10 sudo btrfs subvolume sync /var/tmp >/dev/null 2>&1; SYNC_RC=$?
set -e
echo "host: subvolume sync rc=$SYNC_RC (124 = deletion still PENDING behind the open gofer fds)"
QROW_V1="$(sudo btrfs qgroup show -re --raw --sync -f /var/tmp 2>/dev/null | awk -v id="0/$V1ID" '$1==id' || true)"
echo "host: qgroup row for the deleted unit 0/$V1ID: ${QROW_V1:-<gone>}"
waitAdvance "$A" 8
sig "$A" "$MARK_A" post
echo "-- first 3 post-delete iterations verbatim --"
head -3 "$WORK/$A.post" | sed 's/^/  /'
echo "-- last 2 post-delete iterations verbatim (steady state) --"
tail -2 "$WORK/$A.post" | sed 's/^/  /'
echo "-- HOST-SIDE liveness of the sandbox whose volume was deleted --"
hostCheck "$A"

# ============================ ARM 5a: rm -rf contents ==========================
# A sweep that empties a directory instead of removing the unit. The subvolume —
# and therefore the guest's mount root inode — survives; only the entries die.
# Expectation to falsify: no errors at all, i.e. SILENT data loss with the guest
# happily rewriting its files. Silent is the dangerous outcome.
echo
echo "=============== ARM 5a: rm -rf the volume CONTENTS, subvolume intact ==========="
MARK_B="$(itCount "$B")"
sudo rm -rf "${V2:?}"/*
# NOTE the entry count is racy-by-design: the guest re-creates /fresh every
# iteration, so a non-zero count here is the guest already writing back into the
# emptied volume — which is the finding, not a failure to delete.
echo "host: entries left in V2 immediately after rm: $(sudo ls -A "$V2" | tr '\n' ' ')  (seed was removed)"
waitAdvance "$B" 8
sig "$B" "$MARK_B" post
echo "-- first 3 post-rm iterations verbatim --"; head -3 "$WORK/$B.post" | sed 's/^/  /'
echo "-- host: did the guest re-create its files? --"; sudo ls -A "$V2" | sed 's/^/    /'
hostCheck "$B"

# ============================ ARM 5b: unlink one open file =====================
# The narrowest possible mis-sweep. POSIX says the inode survives until the last
# fd closes; the question is whether the GOFER honours that across the sentry —
# i.e. whether HW/HR keep working on a file with no directory entry.
echo
echo "=============== ARM 5b: unlink ONE file the guest holds open ==================="
MARK_C="$(itCount "$C")"
LS_BEFORE_C="$(grep -oE ' LS=[^ ]*' "$WORK/$C.log" | tail -1)"
sudo rm -f "$V3/held"
echo "host: /held gone? $([ -e "$V3/held" ] && echo NO || echo yes)   LS before was:$LS_BEFORE_C"
waitAdvance "$C" 8
sig "$C" "$MARK_C" post
echo "-- first 3 post-unlink iterations verbatim --"; head -3 "$WORK/$C.post" | sed 's/^/  /'
echo "-- host: did the held file's bytes reappear (fsync through the stale fd)? --"
echo "    /held exists on host: $([ -e "$V3/held" ] && echo YES || echo no)"
hostCheck "$C"

# ============================ ARM 5c: qgroup row only ==========================
# A sweep that reclaims the QUOTA ROW but not the subvolume — plausible because
# deep-dives/the relevant internals page the documented invariant found squota LEAKS a qgroup row per snapshotted unit,
# so the sweep will have a "destroy stale qgroup rows" duty and could mis-target
# a live one. The question is whether that silently UNCAPS a live volume: a
# capacity-cap breach is a the documented invariant accounting failure, not merely data loss.
echo
echo "=============== ARM 5c: destroy the QGROUP ROW only (subvolume intact) ========="
V4ID="$(sudo btrfs subvolume show "$V4" | awk '/Subvolume ID:/{print $3}')"
echo "-- wait for the fill probe to hit the ${CAP_BYTE}B cap --"
for _ in $(seq 1 120); do grep -q 'W=E28/ENOSPC' "$WORK/$D.log" && break; sleep 0.5; done
grep -m1 'W=E28/ENOSPC' "$WORK/$D.log" | sed 's/^/  first ENOSPC: /' || echo "  NO ENOSPC SEEN"
CAP_HIT_TOTAL="$(grep -m1 'W=E28/ENOSPC' "$WORK/$D.log" | grep -oE 'total=[0-9]+' | cut -d= -f2 || true)"
echo "  bytes written when the cap bit: ${CAP_HIT_TOTAL:-none}"
echo "-- host qgroup row before destroy --"; sudo btrfs qgroup show -re --raw --sync -f "$V4" | tail -1
MARK_D="$(itCount "$D")"
set +e
QD_OUT="$(sudo btrfs qgroup destroy "0/$V4ID" /var/tmp 2>&1)"; QD_RC=$?
set -e
echo "btrfs qgroup destroy 0/$V4ID rc=$QD_RC: $QD_OUT"
QROW_V4="$(sudo btrfs qgroup show -re --raw --sync -f /var/tmp 2>/dev/null | awk -v id="0/$V4ID" '$1==id' || true)"
echo "row after destroy attempt: ${QROW_V4:-<gone>}"
waitAdvance "$D" 8
sig "$D" "$MARK_D" postdestroy
tail -2 "$WORK/$D.postdestroy" | sed 's/^/  /'
# Second, distinct probe: whatever `destroy` did, ALSO measure the other shape a
# sweep could take — clearing the limit. This is the one that answers "can it now
# exceed the old cap?" unambiguously.
echo "-- now clear the LIMIT (the other plausible sweep action) --"
MARK_D2="$(itCount "$D")"
set +e
QL_OUT="$(sudo btrfs qgroup limit none "$V4" 2>&1)"; QL_RC=$?
set -e
echo "btrfs qgroup limit none rc=$QL_RC: ${QL_OUT:-<no output>}"
waitAdvance "$D" 10
sig "$D" "$MARK_D2" postlimit
tail -2 "$WORK/$D.postlimit" | sed 's/^/  /'
D_MAX_TOTAL="$(grep -oE 'total=[0-9]+' "$WORK/$D.log" | cut -d= -f2 | sort -n | tail -1)"
echo "  MAX bytes the guest got onto the once-capped volume: $D_MAX_TOTAL (cap was $CAP_BYTE)"
echo "-- host qgroup AFTER --"
sudo btrfs qgroup show -re --raw --sync -f "$V4" 2>&1 | tail -1
hostCheck "$D"

# ============================ ARM 5d: mv the subvolume aside ===================
# The "gentle" sweep: quarantine instead of delete. Renaming a subvolume moves the
# directory entry, not the inode. So does the gofer keep serving the MOVED bytes
# (inode-follow) or re-resolve the path? A DECOY at the old path with different
# content makes the answer unambiguous rather than inferred.
echo
echo "=============== ARM 5d: mv the subvolume aside, decoy at the old path =========="
MARK_E="$(itCount "$E")"
sudo mv "$V5" "$V5M"
sudo mkdir -p "$V5"; printf DECOY | sudo tee "$V5/seed" >/dev/null; sudo chmod 0777 "$V5"
echo "host: moved to $V5M ; DECOY dir now at $V5"
waitAdvance "$E" 8
sig "$E" "$MARK_E" post
echo "-- first 3 post-mv iterations verbatim (SEED tells which dir the guest sees) --"
head -3 "$WORK/$E.post" | sed 's/^/  /'
echo "-- host: where did the guest's later writes land? --"
echo "    moved dir:  $(sudo ls -A "$V5M" | tr '\n' ' ')"
echo "    decoy dir:  $(sudo ls -A "$V5" | tr '\n' ' ')"
hostCheck "$E"

# ============================ ARM 6: the RECOVERY question =====================
# The bit that decides "recoverable with data loss" vs "permanently broken
# sandbox". After a mis-sweep the mount row still says "bind volume X at /vol",
# so the very next resume replays that mount against an absent source. If runsc
# REFUSES to launch, the sandbox can never resume — a mis-sweep is then a
# permanent lifecycle failure requiring operator surgery on the mount table. If
# it silently creates an empty dir, the loss is data-only.
# (Note: this measures the RELAUNCH shape — a cold-start replay of the mount
# config, which is exactly what the documented invariant resume does. It is not runsc C/R.)
echo
echo "=============== ARM 6: relaunch with the bind SOURCE now ABSENT ================"
SHORT_ARGS="[\"/usr/bin/yankprobe\",\"loop\",\"/vol\",\"100\",\"2\"]"
echo "V1 (deleted in arm 1) exists? $([ -e "$V1" ] && echo YES || echo no)"
writeBundle "$WORK/b-f" "$V1" "$SHORT_ARGS"
launchFg "$F" "$WORK/b-f"
echo "6a. relaunch with DELETED source: rc=$(cat "$WORK/$F.rc")"
echo "    stderr: $(tr '\n' ' ' < "$WORK/$F.err" | cut -c1-300)"
echo "    guest:  $(tr '\n' ' ' < "$WORK/$F.log" | cut -c1-300)"
echo "    host: did runsc CREATE the missing source? $([ -e "$V1" ] && echo YES || echo no)"
if [ -e "$V1" ]; then
  echo "      type: $(sudo btrfs subvolume show "$V1" >/dev/null 2>&1 && echo SUBVOLUME || echo PLAIN_DIR)"
  echo "      contents: $(sudo ls -A "$V1" | tr '\n' ' ')"
fi

NEVER="/var/tmp/$TAG-never-existed"
writeBundle "$WORK/b-g" "$NEVER" "$SHORT_ARGS"
# --debug for this one arm only: the surface error ("cannot read client sync
# file: EOF") is the sandbox process dying, which says nothing about WHY. The
# real reason is in the gofer/sentry log, and "which layer refuses" decides
# whether the engine could ever paper over a missing source.
set +e
sudo runsc --root="$RUNSC_ROOT" --platform=systrap --network=none \
  --overlay2=none --file-access=shared --systemd-cgroup=false \
  --debug --debug-log="$WORK/dbg-" \
  run --bundle="$WORK/b-g" "$G" >"$WORK/$G.log" 2>"$WORK/$G.err" </dev/null
echo $? > "$WORK/$G.rc"
set -e
echo "6b. launch with a source that NEVER existed: rc=$(cat "$WORK/$G.rc")"
echo "    stderr: $(tr '\n' ' ' < "$WORK/$G.err" | cut -c1-300)"
# The load-bearing line is the sentry's own FATAL: it names the layer that
# refuses (FS setup, i.e. gofer OpenMount) and the errno. Keep it narrow — a
# broad grep here matches unrelated benign misses like /sys/.../product_name.
echo "    root cause from the debug log (which layer refuses, with the errno):"
CAUSE_6B="$(grep -ohE 'FATAL ERROR: error setting up FS:.*|urpc: RPC call for method goferToHostRPC.OpenMount failed:.*' \
  "$WORK"/dbg-* 2>/dev/null | sed 's/.*\(FATAL\|urpc\)/\1/' | sort -u | head -2 || true)"
echo "${CAUSE_6B:-      <nothing matched>}" | sed 's/^/      /'
echo "    host: created? $([ -e "$NEVER" ] && echo YES || echo no)"
sudo rm -rf "$NEVER" 2>/dev/null || true

# 6c. What a backend's Materialize would do on resume: re-create the unit (empty)
# and relaunch. If this works, a mis-sweep is recoverable-with-data-loss.
sudo btrfs subvolume delete "$V1" >/dev/null 2>&1 || sudo rm -rf "$V1" 2>/dev/null || true
sudo btrfs subvolume create "$V1" >/dev/null
sudo btrfs subvolume show "$V1" | awk '/Subvolume ID:/{print $3}' >> "$QIDS"
sudo chmod 0777 "$V1"
writeBundle "$WORK/b-h" "$V1" "$SHORT_ARGS"
launchFg "$H" "$WORK/b-h"
echo "6c. relaunch after re-creating the unit EMPTY (what Materialize would do): rc=$(cat "$WORK/$H.rc")"
echo "    guest:  $(grep -m2 '^IT=' "$WORK/$H.log" | tr '\n' ' ' | cut -c1-300)"
echo "    stderr: $(tr '\n' ' ' < "$WORK/$H.err" | cut -c1-200)"

# ---- LATE RE-CHECK -------------------------------------------------------------
# Each arm's hostCheck ran seconds after its mutation. A sandbox could in
# principle die LATER (a sentry that faults on a background flush, a deferred
# subvolume deletion finally settling). By now the earliest mutation is minutes
# old, so re-assert liveness — otherwise "the sandbox survives" only means "it
# survived for five seconds".
echo
echo "=============== LATE RE-CHECK: are all five still alive, minutes later? ========"
echo "-- arm 1's deferred deletion: has it settled yet? --"
set +e
timeout 15 sudo btrfs subvolume sync /var/tmp >/dev/null 2>&1; LATE_SYNC_RC=$?
set -e
echo "   subvolume sync rc=$LATE_SYNC_RC (124 = STILL pending behind the live gofer)"
for cid in $A $B $C $D $E; do
  st="$(sudo runsc --root="$RUNSC_ROOT" state "$cid" 2>&1 | tr -d '\n' | grep -oE '"status": "[a-z]+"' || echo '<no state>')"
  echo "   $cid  $st  last_iteration=$(tail -1 "$WORK/$cid.log" | cut -c1-40)"
  echo "$st" > "$WORK/$cid.latestate"
done

# ---- VERDICT -------------------------------------------------------------------
# ASSERTED: the launches worked, the pre-mutation baseline was clean (otherwise
# "it broke" is meaningless), and each arm's post-mutation window actually
# ADVANCED while the container was up (a stall is a third outcome, distinct from
# an errno, and must not be silently read as "fine").
# CLASSIFIED, not asserted: DATA-PLANE vs LIFECYCLE per arm — that classification
# IS the measurement, so the script reports it rather than baking today's answer
# into a pass/fail.
echo; echo "== VERDICT =="
FAIL=0

for cid in $A $B $C $D $E; do
  [ "$(cat "$WORK/$cid.rc")" = 0 ] || { echo "FAIL  launch $cid rc=$(cat "$WORK/$cid.rc")"; FAIL=1; }
done
[ "$FAIL" = 0 ] && echo "PASS  all five live arms launched (rc=0)"

grep -qE '^IT=[0-9]+ W=ok R=ok' "$WORK/$A.pre" \
  && echo "PASS  baseline: the volume was genuinely working before the mutation" \
  || { echo "FAIL  baseline was already broken — the arms below prove nothing"; FAIL=1; }

# classify <label> <cid> — the one call that answers the spike's question.
classify() {
  local label="$1" cid="$2" state execrc n verdict vol alive
  # A probe that ran out its own clock exits, PID 1 dies, the container goes
  # `stopped` — indistinguishable from "the mutation killed it". Refuse to grade.
  if grep -q PROBE_DONE "$WORK/$cid.log"; then
    echo "FAIL  $label: probe reached PROBE_DONE — its budget expired, liveness is not gradeable"
    FAIL=1; return 0
  fi
  state="$(cat "$WORK/$cid.state" 2>/dev/null || echo missing)"
  execrc="$(cat "$WORK/$cid.execrc" 2>/dev/null || echo 99)"
  n="$(wc -l < "$WORK/$cid.$3" 2>/dev/null || echo 0)"
  case "$state" in *'"status": "running"'*) alive=yes;; *) alive=no;; esac
  if [ "$alive" = yes ] && [ "$execrc" = 0 ]; then
    verdict="DATA-PLANE (sandbox running + fresh exec rc=0)"
  else
    verdict="LIFECYCLE (alive=$alive exec_rc=$execrc)"
    FAIL=1   # a dead sandbox contradicts every other arm measured here
  fi
  # The late re-check must agree with the arm-time check, or "survives" was only
  # "survived for five seconds".
  if ! grep -q 'running' "$WORK/$cid.latestate" 2>/dev/null; then
    verdict="$verdict  BUT DIED LATER (late state: $(cat "$WORK/$cid.latestate" 2>/dev/null))"
    FAIL=1
  fi
  vol="$(grep -oE '(^| )W=[^ ]*' "$WORK/$cid.$3" 2>/dev/null | sed 's/^ //' | sort -u | tr '\n' ',' || true)"
  if [ "$n" -lt 3 ]; then
    echo "FAIL  $label: probe advanced only $n iterations after the mutation (HANG?) — $verdict"
    FAIL=1
  else
    echo "CLASS $label: $verdict ; volume write states after = ${vol%,}"
  fi
}

classify "arm1  subvolume delete " "$A" post
classify "arm5a rm -rf contents  " "$B" post
classify "arm5b unlink open file " "$C" post
classify "arm5c qgroup row       " "$D" postlimit
classify "arm5d mv aside         " "$E" post

echo
echo "-- recovery (the recoverable-vs-broken bit) --"
echo "  6a relaunch, source DELETED      : rc=$(cat "$WORK/$F.rc")"
echo "  6b launch,   source NEVER existed: rc=$(cat "$WORK/$G.rc")"
echo "  6c relaunch, unit RE-CREATED     : rc=$(cat "$WORK/$H.rc")"
[ "$(cat "$WORK/$H.rc")" = 0 ] && grep -q '^IT=1 W=ok' "$WORK/$H.log" \
  && echo "PASS  re-creating the unit restores a launchable sandbox ⇒ mis-sweep is RECOVERABLE WITH DATA LOSS" \
  || { echo "NOTE  re-created unit did NOT yield a clean launch — see 6c above"; FAIL=1; }

echo
echo "-- arm 5c capacity-cap consequence --"
echo "  cap=$CAP_BYTE  first-ENOSPC-at=${CAP_HIT_TOTAL:-none}  max-written=$D_MAX_TOTAL"
if [ -n "${D_MAX_TOTAL:-}" ] && [ "$D_MAX_TOTAL" -gt "$CAP_BYTE" ]; then
  echo "  ⇒ the guest EXCEEDED the old cap after the quota row/limit was swept"
else
  echo "  ⇒ the cap held; sweeping the quota row did not uncap the live volume"
fi

exit $FAIL
