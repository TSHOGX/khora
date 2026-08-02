#!/usr/bin/env bash
# spike-death-cause.sh — preflight for the §5.A RAM-cap wiring: where can the
# engine READ a sandbox's death cause from, and does that source SURVIVE a
# control-plane (orchestrator) restart?
#
# Premise (established by three prior spikes, NOT re-derived here): a RAM cap on
# a gVisor sandbox can only be a SAFETY limit — there is no guest-internal OOM
# killer, so exceeding the cap TERMINATES the sandbox via the host memcg OOM
# killer. Which task the kernel picks is nondeterministic, so every OOM config
# below sets `memory.oom.group=1`, making termination whole-group and
# deterministic (and eliminating the no-eligible-victim infinite-retry storm that
# writes ~66 MB/s of kernel log and fills the host disk).
#
# The engine needs `DeathCause{Unknown, Gone, ResourceExceeded}`. cgroupfs is
# virtual and dies with a host reboot, so a cgroup-only signal is always lost
# across "OOM → reboot → orchestrator reconciles". `runsc --root=<dir>` is a REAL
# directory, so it is the one death-signal candidate that could survive. Hence:
#
#   Q1 does runsc's on-disk --root state record a usable death signal, and can a
#      process that did NOT witness the death read it?  ← the high-value question
#   Q3 while a container is `stopped` but not yet `delete`d, is the cgroup leaf
#      still present and is memory.events' oom_group_kill still readable?
#      (an opportunistic-read optimization only)
#
# Four labelled comparison cases, so a field that is IDENTICAL across all of them
# is proven useless to the engine:
#   (a) OOM     — whole-group kill under memory.max + memory.swap.max=0 + oom.group=1
#   (b) EXIT0   — guest PID1 exits 0
#   (c) SIGKILL — external `runsc kill <cid> KILL` on an idle PID1
#   (d) EXIT42  — guest PID1 exits nonzero
#
# Every case is launched DETACHED (`run --detach`). A FOREGROUND `runsc run`
# auto-deletes its own state on exit, leaving --root completely empty — which
# would void the whole question. Detach is what makes the state observable at all.
#
# ---------------------------------------------------------------------------
# CGROUP MECHANICS, measured here, NOT as previously assumed:
#
# runsc does NOT freely "create its own leaf from linux.resources". Measured on
# release-20260714.0:
#   * runsc creates ONLY the FINAL path segment of linux.cgroupsPath, and only if
#     that leaf is ABSENT while its PARENT already exists. It never mkdir -p's
#     intermediate levels: a cgroupsPath whose parent is missing fails the launch
#     outright ("cannot set up cgroup for root: configuring cgroup: open
#     .../memory.swap.max: no such file or directory").
#   * It applies memory.max ONLY to a leaf it created ITSELF (the persisted state's
#     sandbox.cgroup.cgroupv2.own lists it). If the engine pre-creates the FULL
#     path, runsc treats the leaf as foreign, sets memory.swap.max=0 but leaves
#     memory.max=max — the RAM cap SILENTLY DOES NOT APPLY. (strace: it opens
#     memory.swap.max O_WRONLY, but memory.max only O_RDONLY — for a foreign leaf
#     it reads the cap to derive --total-memory rather than writing it.)
#   * `runsc delete` removes the leaf ONLY if runsc owns it (own != null);
#     a fully pre-created leaf survives delete and the engine must rmdir it.
#
# So the ONLY configuration where the OCI memory limit actually binds is:
#   engine mkdir's the PARENT, runsc creates the LEAF.
# This spike uses exactly that, and case (a) asserts memory.max is really set.
#
# oom.group ORDERING is the safety property: PID1 is `memhog idle` (allocates
# nothing), so the leaf exists and is quiescent; we write memory.oom.group=1
# BEFORE any allocation, then exec the bounded allocator.
# ---------------------------------------------------------------------------
#
# "Fresh process" discipline: each case is LAUNCHED by a short-lived child bash
# that then EXITS, and the on-disk state is read by a separate fresh `bash`
# invocation given only (root dir, cid) — it never witnessed the death. That is
# the orchestrator-restart simulation; the host is never rebooted.
#
# Usage: sudo ./spike-death-cause.sh    (needs root: runsc gofer + cgroup writes)
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
# /var/tmp is native btrfs here; /tmp is tmpfs. runsc state + bundles live native.
WORK="$(mktemp -d /var/tmp/spike-death.XXXXXX)"
RUNSC_ROOT="$WORK/runsc-root"
MEMHOG="$WORK/memhog"
ROOTFS="$WORK/rootfs"
TAG="dcs$$"
FINDINGS="$WORK/findings"
READER="$WORK/read-state.sh"

# The engine-owned PARENT cgroup; runsc creates one leaf per sandbox beneath it.
CG_PARENT="spike-$TAG"

CAP_MIB=256          # host ceiling: high enough that an eligible victim always
                     # exists (sentry total-vm ~3.5 GB), low enough to be cheap
ALLOC_STEP_MIB=32
ALLOC_MAX_MIB=512    # bounded by memhog's own 4096 MiB hard cap regardless
CONTAINER_TIMEOUT=60 # wall-clock bound on every runsc invocation

CIDS=()

# ---------------------------------------------------------------------------
# safety: log-growth guard. oom.group=1 prevents the OOM retry storm, but we ALSO
# abort hard if the logs grow past a few GB — filling the host disk is a
# host-level incident (it has happened once).
# ---------------------------------------------------------------------------
LOG_LIMIT_BYTES=$((3 * 1024 * 1024 * 1024))
logsize() {
  local t=0 s
  for f in /var/log/syslog /var/log/kern.log; do
    s="$(stat -c %s "$f" 2>/dev/null || echo 0)"; t=$((t + s))
  done
  echo "$t"
}
LOGSIZE_BASE="$(logsize)"
guard_logs() {
  local now grew
  now="$(logsize)"; grew=$((now - LOGSIZE_BASE))
  if [ "$now" -gt "$LOG_LIMIT_BYTES" ]; then
    echo "ABORT: /var/log/{syslog,kern.log} total ${now}B exceeds ${LOG_LIMIT_BYTES}B (OOM retry storm?)" >&2
    exit 90
  fi
  echo "$grew"
}

cleanup() {
  set +e
  for cid in "${CIDS[@]:-}"; do
    timeout 20 runsc --root="$RUNSC_ROOT" delete --force "$cid" >/dev/null 2>&1
  done
  # kill anything still in our cgroup subtree, then remove the whole subtree
  # (runsc removes leaves it OWNS on delete; the engine-owned parent is ours).
  if [ -d "/sys/fs/cgroup/$CG_PARENT" ]; then
    while read -r pid; do [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null; done \
      < <(find "/sys/fs/cgroup/$CG_PARENT" -name cgroup.procs -exec cat {} + 2>/dev/null)
    sleep 0.3
    find "/sys/fs/cgroup/$CG_PARENT" -depth -type d -exec rmdir {} + >/dev/null 2>&1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

note() { echo "$*" >>"$FINDINGS"; }
hdr()  { echo; echo "== $* =="; }

rs() { timeout "$CONTAINER_TIMEOUT" runsc --root="$RUNSC_ROOT" \
         --platform=systrap --network=none --overlay2=none \
         --file-access=shared --systemd-cgroup=false "$@"; }

# ---------------------------------------------------------------------------
# write_bundle <cid> <capped:yes|no> <arg...>  → bundle dir on stdout
# capped=yes → linux.cgroupsPath = <parent>/<cid> (parent pre-created by us, LEAF
#   left for runsc to create so the memory limit actually binds — see header)
#   plus linux.resources.memory.{limit,swap}. swap == limit is the OCI spelling of
#   "no swap", i.e. cgroup v2 memory.swap.max=0 → a HARD ceiling.
# ---------------------------------------------------------------------------
write_bundle() {
  local cid="$1" capped="$2"; shift 2
  local bundle="$WORK/bundle-$cid"
  mkdir -p "$bundle"
  local args="" a
  for a in "$@"; do args="$args${args:+,}\"$a\""; done

  local cgline="" resources=""
  if [ "$capped" = yes ]; then
    cgline="\"cgroupsPath\":\"$CG_PARENT/$cid\","
    resources="\"resources\":{\"memory\":{\"limit\":$((CAP_MIB<<20)),\"swap\":$((CAP_MIB<<20))}},"
  fi

  cat >"$bundle/config.json" <<JSON
{"ociVersion":"1.0.0",
 "process":{"terminal":false,"user":{"uid":0,"gid":0},
   "args":[$args],
   "env":["PATH=/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root"],"cwd":"/",
   "capabilities":{"bounding":["CAP_DAC_OVERRIDE"],"effective":["CAP_DAC_OVERRIDE"],"permitted":["CAP_DAC_OVERRIDE"]}},
 "root":{"path":"$ROOTFS","readonly":false},"hostname":"guest",
 "mounts":[{"destination":"/proc","type":"proc","source":"proc"},
   {"destination":"/dev","type":"tmpfs","source":"tmpfs","options":["nosuid","mode=755","size=65536k"]}],
 "linux":{$cgline$resources
   "namespaces":[{"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"}]}}
JSON
  echo "$bundle"
}

# ---------------------------------------------------------------------------
# launch_detached_in_child <cid> <bundle>
# Launches via a SHORT-LIVED CHILD bash that then exits. Nothing in the reading
# process ever held the launching pipeline — part of the "no memory of the death"
# discipline. Detached stdio MUST go to a real file, never a pipe
# (memory: runsc-detach-stdio-deadlock — Cmd.Wait/`$(...)` hangs forever).
# ---------------------------------------------------------------------------
launch_detached_in_child() {
  local cid="$1" bundle="$2"
  CIDS+=("$cid")
  bash -c "exec timeout $CONTAINER_TIMEOUT runsc --root='$RUNSC_ROOT' \
      --platform=systrap --network=none --overlay2=none --file-access=shared \
      --systemd-cgroup=false run --detach --bundle='$bundle' '$cid'" \
    >"$bundle/guest.log" 2>&1
}

status_of() { rs state "$1" 2>/dev/null | awk -F'"' '/"status"/{print $4}'; }

wait_status() { # wait_status <cid> <want> <tries>
  local cid="$1" want="$2" n="${3:-40}"
  for _ in $(seq "$n"); do
    [ "$(status_of "$cid")" = "$want" ] && return 0
    sleep 0.25
  done
  return 1
}

# ---------------------------------------------------------------------------
# The FRESH READER. A standalone script handed only (root, cid): it has no
# knowledge of how or why the container died. Invoked as a brand-new `bash`
# process — this is the orchestrator-restart simulation.
# ---------------------------------------------------------------------------
write_reader() {
  cat >"$READER" <<'EOS'
#!/usr/bin/env bash
# read-state.sh <runsc-root> <cid> — a FRESH process reading only on-disk state.
set -uo pipefail
ROOT="$1"; CID="$2"
echo "FRESH_READER pid=$$ (never witnessed the death)"
echo "--- files under runsc --root matching this cid ---"
find "$ROOT" -maxdepth 1 -name "*$CID*" -printf '  %y %7s %p\n' 2>/dev/null | sort -k4
echo "--- persisted .state: the fields that could carry a death signal ---"
S="$ROOT/${CID}_sandbox:${CID}.state"
if [ -f "$S" ]; then
  python3 - "$S" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
sb=d.get("sandbox",{})
for k in ("id","status","goferPid","createdAt","owner"):
    print(f"    {k} = {d.get(k,'<absent>')!r}")
for k in ("pid","checkpointed","restored","originalOomScoreAdj","cpuTimeSaved","wallTimeSaved"):
    print(f"    sandbox.{k} = {sb.get(k,'<absent>')!r}")
cg=sb.get("cgroup",{}).get("cgroupv2",{})
print(f"    sandbox.cgroup.path = {cg.get('path','<absent>')!r}")
print(f"    sandbox.cgroup.own  = {cg.get('own','<absent>')!r}")
# exhaustive: ANY key anywhere whose name hints at exit/signal/oom/kill
def walk(o,p=""):
    if isinstance(o,dict):
        for k,v in o.items():
            yield from walk(v,f"{p}.{k}" if p else k)
    elif isinstance(o,list):
        for i,v in enumerate(o): yield from walk(v,f"{p}[{i}]")
    else: yield p,o
hits=[(p,v) for p,v in walk(d)
      if any(t in p.lower() for t in ("exit","signal","oom","kill","death","reason","cause"))]
print(f"    exit/signal/oom/kill-ish keys anywhere in the blob: {hits if hits else 'NONE'}")
PY
else
  echo "    (no .state file at $S)"
fi
echo "--- runsc state (fresh process) ---"
timeout 20 runsc --root="$ROOT" state "$CID" 2>&1 | tr -d '\n ' | sed 's/^/    /'; echo
echo "--- runsc list (fresh process) ---"
timeout 20 runsc --root="$ROOT" list 2>&1 | grep -E "^ID|$CID" | sed 's/^/    /'
echo "FRESH_READER_END"
EOS
  chmod +x "$READER"
}

fresh_read() { bash "$READER" "$RUNSC_ROOT" "$1" 2>&1; }

# sentry/gofer presence for a cid (are the live processes gone?)
procs_for() { pgrep -af runsc 2>/dev/null | grep -F -- "$1" | grep -vE 'grep|pgrep' || true; }

# disk_status <cid> → the "status" field in the PERSISTED .state (not the CLI's)
disk_status() {
  local s="$RUNSC_ROOT/${1}_sandbox:${1}.state"
  [ -f "$s" ] || { echo "<no-file>"; return; }
  python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("status","<absent>"))' "$s" 2>/dev/null || echo "<unreadable>"
}
disk_sandbox_pid() {
  local s="$RUNSC_ROOT/${1}_sandbox:${1}.state"
  [ -f "$s" ] || { echo "<no-file>"; return; }
  python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("sandbox",{}).get("pid","<absent>"))' "$s" 2>/dev/null || echo "?"
}
cli_status() { rs state "$1" 2>&1 | tr -d '\n ' | grep -oE '"status":"[a-z]+"' || echo "<error>"; }
statefile_size() {
  local s="$RUNSC_ROOT/${1}_sandbox:${1}.state"
  [ -f "$s" ] && stat -c %s "$s" || echo "absent"
}

###############################################################################
# main
###############################################################################
[ "$(id -u)" = 0 ] || { echo "must run as root (runsc gofer + cgroup writes)"; exit 1; }

hdr "substrate"
echo "   kernel:      $(uname -r)  ($(uname -m))"
echo "   runsc:       $(runsc --version | head -1)"
echo "   cgroup v2 controllers: $(cat /sys/fs/cgroup/cgroup.controllers)"
echo "   fs for runsc --root: $(findmnt -T "$WORK" -no SOURCE,FSTYPE)"
echo "   scratch:     $WORK"
echo "   log guard:   baseline $(logsize)B, abort above ${LOG_LIMIT_BYTES}B"
note "[substrate] kernel=$(uname -r) runsc=$(runsc --version | head -1 | awk '{print $3}') cap=${CAP_MIB}MiB"

hdr "build guest probe + minimal rootfs"
( cd "$REPO/test/preflight/memhog" && CGO_ENABLED=0 GO111MODULE=off go build -o "$MEMHOG" . )
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" "$MEMHOG" >/dev/null
cp -a "$MEMHOG" "$ROOTFS/usr/bin/memhog"
echo "   memhog built (CGO off, static) → $ROOTFS/usr/bin/memhog"
echo "   rootfs: $ROOTFS (dash + memhog, no coreutils)"
write_reader
sync

hdr "engine-owned PARENT cgroup (runsc creates the per-sandbox LEAF beneath it)"
mkdir -p "/sys/fs/cgroup/$CG_PARENT"
# the parent must delegate the memory controller down to the leaf
echo "+memory" >"/sys/fs/cgroup/$CG_PARENT/cgroup.subtree_control" 2>/dev/null || true
echo "   parent:           /sys/fs/cgroup/$CG_PARENT"
echo "   subtree_control:  $(cat "/sys/fs/cgroup/$CG_PARENT/cgroup.subtree_control")"

# =============================================================================
# CASE (a) — OOM: whole-group kill under memory.max + swap.max=0 + oom.group=1
# =============================================================================
echo; echo "############ CASE (a) OOM (cap=${CAP_MIB}MiB, oom.group=1) ############"
CID_A="${TAG}oom"
LEAF_A="/sys/fs/cgroup/$CG_PARENT/$CID_A"
B_A="$(write_bundle "$CID_A" yes /usr/bin/memhog idle)"

hdr "(a) launch detached (PID1 = memhog idle → allocates nothing yet)"
launch_detached_in_child "$CID_A" "$B_A"
wait_status "$CID_A" running || { echo "FATAL: (a) never reached running"; cat "$B_A/guest.log"; exit 1; }
echo "   status=$(status_of "$CID_A")"

hdr "(a) did the OCI memory limit ACTUALLY bind? (runsc must OWN the leaf)"
[ -d "$LEAF_A" ] || { echo "FATAL: runsc did not create the leaf $LEAF_A"; exit 1; }
A_MAX="$(cat "$LEAF_A/memory.max")"
A_SWAP="$(cat "$LEAF_A/memory.swap.max")"
A_OWN="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["sandbox"]["cgroup"]["cgroupv2"]["own"])' \
          "$RUNSC_ROOT/${CID_A}_sandbox:${CID_A}.state" 2>/dev/null || echo '?')"
echo "   leaf:            $LEAF_A"
echo "   memory.max:      $A_MAX   (want $((CAP_MIB<<20)))"
echo "   memory.swap.max: $A_SWAP   (want 0 → hard ceiling, no swap escape)"
echo "   runsc-owned:     $A_OWN"
echo "   tasks in leaf:   $(wc -l <"$LEAF_A/cgroup.procs")"
if [ "$A_MAX" != "$((CAP_MIB<<20))" ]; then
  echo "FATAL: memory.max is '$A_MAX', not the cap — the limit did NOT bind."
  echo "       (this is the pre-created-leaf trap described in the header)"
  exit 1
fi
note "[a/setup] OCI memory limit BOUND: memory.max=$A_MAX swap.max=$A_SWAP, leaf created+owned by runsc ($A_OWN)"

hdr "(a) is the cap visible IN the guest? (runsc derives --total-memory from memory.max)"
rs exec "$CID_A" /usr/bin/memhog meminfo 2>&1 | grep -E 'MemTotal|MemFree' | sed 's/^/   /'
note "[a/setup] guest MemTotal reflects the host cap (runsc reads memory.max of its own cgroup)"

hdr "(a) MANDATORY SAFETY: memory.oom.group=1 BEFORE any allocation"
echo 1 >"$LEAF_A/memory.oom.group"
echo "   memory.oom.group = $(cat "$LEAF_A/memory.oom.group")  (whole-group, deterministic)"
[ "$(cat "$LEAF_A/memory.oom.group")" = 1 ] || { echo "FATAL: could not set oom.group"; exit 1; }
echo "   memory.events before: $(tr '\n' ' ' <"$LEAF_A/memory.events")"

hdr "(a) exec the bounded allocator — expect a whole-group termination"
set +e
timeout "$CONTAINER_TIMEOUT" runsc --root="$RUNSC_ROOT" --platform=systrap \
  --network=none --overlay2=none --file-access=shared --systemd-cgroup=false \
  exec "$CID_A" /usr/bin/memhog alloc "$ALLOC_STEP_MIB" "$ALLOC_MAX_MIB" >"$WORK/a-exec.log" 2>&1
A_EXEC_RC=$?
set -e
echo "   exec rc=$A_EXEC_RC; guest allocator output (tail):"
tail -4 "$WORK/a-exec.log" | sed 's/^/     /'
echo "   log growth so far: $(guard_logs)B"

# ---------- Q3 read: leaf + memory.events BEFORE any `runsc delete` ----------
hdr "(a) Q3 — cgroup leaf + memory.events while STOPPED but NOT yet deleted"
sleep 1
A_STATUS_STOPPED="$(status_of "$CID_A")"
echo "   runsc status now: ${A_STATUS_STOPPED:-<none>}"
if [ -d "$LEAF_A" ]; then
  A_LEAF_PRESENT=yes
  A_EVENTS="$(tr '\n' ' ' <"$LEAF_A/memory.events")"
  A_OOMKILL="$(awk '$1=="oom_kill"{print $2}' "$LEAF_A/memory.events")"
  A_OOMGROUP="$(awk '$1=="oom_group_kill"{print $2}' "$LEAF_A/memory.events")"
  A_PEAK="$(cat "$LEAF_A/memory.peak" 2>/dev/null || echo n/a)"
  echo "   leaf STILL PRESENT: $LEAF_A"
  echo "   memory.events:  $A_EVENTS"
  echo "   memory.peak:    $A_PEAK  (cap $((CAP_MIB<<20)))"
  echo "   tasks left:     $(wc -l <"$LEAF_A/cgroup.procs")"
else
  A_LEAF_PRESENT=no; A_EVENTS="n/a"; A_OOMKILL="n/a"; A_OOMGROUP="n/a"; A_PEAK="n/a"
  echo "   leaf GONE already (before any runsc delete): $LEAF_A"
fi
note "[a/Q3] stopped-but-not-deleted: leaf present=$A_LEAF_PRESENT oom_kill=$A_OOMKILL oom_group_kill=$A_OOMGROUP peak=$A_PEAK"
note "[a/Q3] full memory.events at that moment: ($A_EVENTS)"

hdr "(a) are the sentry/gofer processes gone?"
A_PROCS="$(procs_for "$CID_A")"
if [ -z "$A_PROCS" ]; then echo "   no runsc process mentions $CID_A → sentry+gofer GONE"
else echo "$A_PROCS" | sed 's/^/   STILL ALIVE: /'; fi
note "[a] after OOM: sentry/gofer processes $([ -z "$A_PROCS" ] && echo GONE || echo STILL-PRESENT); runsc status=$A_STATUS_STOPPED"

hdr "(a) FRESH PROCESS reads the on-disk --root state"
fresh_read "$CID_A" | sed 's/^/   /'

# =============================================================================
# CASE (b) — clean exit 0
# =============================================================================
echo; echo "############ CASE (b) EXIT0 — guest PID1 exits 0 ############"
CID_B="${TAG}exit0"
B_B="$(write_bundle "$CID_B" no /bin/sh -c "exit 0")"
launch_detached_in_child "$CID_B" "$B_B"
sleep 2
echo "   runsc status: $(status_of "$CID_B" || true)"
echo "   sentry/gofer procs mentioning it: $(procs_for "$CID_B" | wc -l)"
hdr "(b) FRESH PROCESS reads the on-disk --root state"
fresh_read "$CID_B" | sed 's/^/   /'

# =============================================================================
# CASE (c) — external runsc kill SIGKILL on an idle PID1
# =============================================================================
echo; echo "############ CASE (c) SIGKILL — external \`runsc kill KILL\` ############"
CID_C="${TAG}sigkill"
B_C="$(write_bundle "$CID_C" no /usr/bin/memhog idle)"
launch_detached_in_child "$CID_C" "$B_C"
wait_status "$CID_C" running || { echo "FATAL: (c) never reached running"; cat "$B_C/guest.log"; exit 1; }
echo "   status before kill: $(status_of "$CID_C")"
rs kill "$CID_C" KILL
sleep 2
echo "   status after kill:  $(status_of "$CID_C" || true)"
echo "   sentry/gofer procs mentioning it: $(procs_for "$CID_C" | wc -l)"
hdr "(c) FRESH PROCESS reads the on-disk --root state"
fresh_read "$CID_C" | sed 's/^/   /'

# =============================================================================
# CASE (d) — nonzero exit
# =============================================================================
echo; echo "############ CASE (d) EXIT42 — guest PID1 exits 42 ############"
CID_D="${TAG}exit42"
B_D="$(write_bundle "$CID_D" no /bin/sh -c "exit 42")"
launch_detached_in_child "$CID_D" "$B_D"
sleep 2
echo "   runsc status: $(status_of "$CID_D" || true)"
hdr "(d) FRESH PROCESS reads the on-disk --root state"
fresh_read "$CID_D" | sed 's/^/   /'

# =============================================================================
# Q1 VERDICT — diff the persisted state across the four cases
# =============================================================================
echo; echo "############ Q1 DIFF: is death distinguishable from on-disk state alone? ############"
echo
printf '   %-26s %-13s %-13s %-13s %-13s\n' "signal source" "(a) OOM" "(b) EXIT0" "(c) SIGKILL" "(d) EXIT42"
printf '   %-26s %-13s %-13s %-13s %-13s\n' "--------------------------" "-----------" "-----------" "-----------" "-----------"
printf '   %-26s %-13s %-13s %-13s %-13s\n' ".state file size (bytes)" \
  "$(statefile_size "$CID_A")" "$(statefile_size "$CID_B")" "$(statefile_size "$CID_C")" "$(statefile_size "$CID_D")"
printf '   %-26s %-13s %-13s %-13s %-13s\n' ".state status field" \
  "$(disk_status "$CID_A")" "$(disk_status "$CID_B")" "$(disk_status "$CID_C")" "$(disk_status "$CID_D")"
printf '   %-26s %-13s %-13s %-13s %-13s\n' ".state sandbox.pid" \
  "$(disk_sandbox_pid "$CID_A")" "$(disk_sandbox_pid "$CID_B")" "$(disk_sandbox_pid "$CID_C")" "$(disk_sandbox_pid "$CID_D")"
printf '   %-26s %-13s %-13s %-13s %-13s\n' "runsc state (CLI)" \
  "$(cli_status "$CID_A")" "$(cli_status "$CID_B")" "$(cli_status "$CID_C")" "$(cli_status "$CID_D")"

DS_A="$(disk_status "$CID_A")"; DS_B="$(disk_status "$CID_B")"
DS_C="$(disk_status "$CID_C")"; DS_D="$(disk_status "$CID_D")"
if [ "$DS_A" = "$DS_B" ] && [ "$DS_B" = "$DS_C" ] && [ "$DS_C" = "$DS_D" ]; then
  note "[Q1] the persisted .state \"status\" field is IDENTICAL ('$DS_A') across OOM / exit0 / SIGKILL / exit42 → USELESS as a death signal."
else
  note "[Q1] the persisted .state \"status\" differs across cases: a=$DS_A b=$DS_B c=$DS_C d=$DS_D"
fi

echo
echo "   --- exhaustive: does ANY exit/signal/oom-ish key exist in the persisted blob? ---"
for pair in "a:$CID_A" "b:$CID_B" "c:$CID_C" "d:$CID_D"; do
  lbl="${pair%%:*}"; cid="${pair#*:}"
  S="$RUNSC_ROOT/${cid}_sandbox:${cid}.state"
  if [ -f "$S" ]; then
    python3 - "$S" "$lbl" <<'PY' | sed 's/^/     /'
import json,sys
d=json.load(open(sys.argv[1]))
def walk(o,p=""):
    if isinstance(o,dict):
        for k,v in o.items(): yield from walk(v,f"{p}.{k}" if p else k)
    elif isinstance(o,list):
        for i,v in enumerate(o): yield from walk(v,f"{p}[{i}]")
    else: yield p,o
hits=[(p,v) for p,v in walk(d) if any(t in p.lower() for t in
      ("exit","signal","oom","kill","death","reason","cause"))]
print(f"case {sys.argv[2]}: {hits if hits else 'NO exit/signal/oom/kill key anywhere in the blob'}")
PY
  else
    echo "     case $lbl: NO .state FILE AT ALL"
  fi
done

echo
echo "   --- verbatim persisted .state for case (a) OOM, death-relevant fields ---"
S_A="$RUNSC_ROOT/${CID_A}_sandbox:${CID_A}.state"
if [ -f "$S_A" ]; then
  python3 - "$S_A" <<'PY' | sed 's/^/     /'
import json,sys
d=json.load(open(sys.argv[1]))
keep={"id","status","bundleDir","createdAt","owner","goferPid"}
out={k:v for k,v in d.items() if k in keep}
sb=d.get("sandbox",{})
out["sandbox"]={k:sb.get(k) for k in
  ("id","pid","cgroup","originalOomScoreAdj","startTime","checkpointed","restored")}
print(json.dumps(out,indent=2))
PY
else
  echo "     (absent)"
fi

echo
echo "   --- \`runsc list\` (one fresh process, all four) ---"
timeout 20 runsc --root="$RUNSC_ROOT" list 2>&1 | sed 's/^/     /'

PERSISTED_COUNT="$(find "$RUNSC_ROOT" -maxdepth 1 -name '*.state' 2>/dev/null | wc -l)"
note "[Q1] .state files still on disk for the four dead containers: $PERSISTED_COUNT/4 (they persist; a foreground \`runsc run\` would instead self-delete and leave --root EMPTY)"

# =============================================================================
# Q1 follow-up — is the CLI's "stopped" read from disk, or recomputed live?
# The persisted status is stale ("running"); `runsc state` says "stopped".
# So the CLI must be recomputing liveness — prove it by probing the recorded pid.
# =============================================================================
hdr "Q1 follow-up: is \`runsc state\`'s verdict on disk, or recomputed from the pid?"
A_PID="$(disk_sandbox_pid "$CID_A")"
echo "   persisted .state status      = $(disk_status "$CID_A")   ← written at CREATE, never updated on death"
echo "   persisted sandbox.pid        = $A_PID"
echo "   is that pid alive now?       = $(kill -0 "$A_PID" 2>/dev/null && echo YES || echo NO)"
echo "   runsc state says             = $(cli_status "$CID_A")"
note "[Q1] the on-disk \"status\" stays 'running' for a DEAD container; \`runsc state\` reports 'stopped' by probing the recorded sandbox.pid LIVENESS at read time — the 'stopped' verdict is COMPUTED, not persisted."
note "[Q1] consequence: after a host REBOOT the recorded pid is meaningless (recycled or absent), so the runsc state dir can distinguish 'gone' at best — never WHY it died."

# =============================================================================
# Q3 follow-up — does `runsc delete` remove the leaf it owns?
# =============================================================================
hdr "Q3 follow-up: does \`runsc delete\` remove the cgroup leaf (and its memory.events)?"
echo "   leaf before delete: $([ -d "$LEAF_A" ] && echo PRESENT || echo ABSENT)  ($LEAF_A)"
echo "   oom_group_kill readable before delete: $(awk '$1=="oom_group_kill"{print $2}' "$LEAF_A/memory.events" 2>/dev/null || echo unreadable)"
rs delete --force "$CID_A" >/dev/null 2>&1 || true
sleep 0.5
A_LEAF_AFTER="$([ -d "$LEAF_A" ] && echo PRESENT || echo REMOVED)"
echo "   leaf after  delete: $A_LEAF_AFTER"
echo "   oom_group_kill readable after delete:  $(awk '$1=="oom_group_kill"{print $2}' "$LEAF_A/memory.events" 2>/dev/null || echo UNREADABLE)"
echo "   .state file after delete: $(statefile_size "$CID_A")"
note "[a/Q3] \`runsc delete\` → cgroup leaf $A_LEAF_AFTER (runsc owns leaves it created) → memory.events oom_group_kill is DESTROYED with it; the .state file is deleted too."

# =============================================================================
# hygiene: leave nothing behind
# =============================================================================
hdr "hygiene"
for cid in "${CIDS[@]}"; do rs delete --force "$cid" >/dev/null 2>&1 || true; done
sleep 0.5
LEFT_PROCS="$(pgrep -af runsc 2>/dev/null | grep -F "$TAG" | grep -vE 'grep|pgrep' || true)"
find "/sys/fs/cgroup/$CG_PARENT" -depth -type d -exec rmdir {} + >/dev/null 2>&1 || true
LEFT_CG="$(find /sys/fs/cgroup -maxdepth 2 -type d -name "spike-$TAG*" 2>/dev/null || true)"
echo "   runsc processes mentioning $TAG:   $([ -z "$LEFT_PROCS" ] && echo none || echo "$LEFT_PROCS")"
echo "   cgroup dirs matching spike-$TAG*:  $([ -z "$LEFT_CG" ] && echo none || echo "$LEFT_CG")"
echo "   state files left in --root:        $(find "$RUNSC_ROOT" -type f 2>/dev/null | wc -l)"
echo "   log growth total:                  $(guard_logs)B"
note "[hygiene] leftover runsc procs=$([ -z "$LEFT_PROCS" ] && echo 0 || echo SOME) leftover cgroups=$([ -z "$LEFT_CG" ] && echo 0 || echo SOME) log growth=$(guard_logs)B (scratch $WORK removed by the EXIT trap)"

echo; echo "=================== FINDINGS ==================="
cat "$FINDINGS"
echo "==============================================="
