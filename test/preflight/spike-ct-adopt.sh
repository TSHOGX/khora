#!/usr/bin/env bash
# spike-ct-adopt.sh — measures whether an established flow survives what an
# Adopt (atomic transition rollback) may do to a sandbox's rule set after a control-plane restart.
#
# This was registered in network substrate measurements as blocked-unmeasured ("no `conntrack` CLI
# installed on `sbx`"); conntrack-tools 1.4.9 is now installed, the measurement is
# done, and the result is recorded here — so this script is the reproduction, not the
# open question. Re-run it if the Adopt implementation changes.
# NOTE this machine has NO `/proc/net/nf_conntrack` (CONFIG_NF_CONNTRACK_PROCFS
# is unset — the file does not exist even for root), so the netlink CLI was the
# only avenue; there was no package-free fallback.
#
# ── The question, split into the mechanisms that can actually break a flow ─────
#
# Adopt takes a LIVE placement back over after an orchestrator restart.
# A sandbox's nft table is crash-surviving host state, exactly like its netns, so
# Adopt has to decide what to do with the table it finds. Three candidate actions,
# and the worry is whether any of them kills a flow that is mid-transfer:
#
#   B  reinstall the same table atomically (the idempotent bare-`table` +
#      `delete` + define triple in one `nft -f`) — the obvious
#      "re-render and re-apply, don't reason about what's there" Adopt.
#   C  delete the table, then install (a non-atomic Adopt, or a
#      ReclaimOrphans/Adopt race: the table is briefly absent).
#   D  clear conntrack state (`conntrack -F`) — which nothing in the spec asks
#      for, but which a "clean slate on restart" reflex would reach for.
#
# Prior egress measurements showed that removing the `established,related` arm breaks even a
# permitted flow: the reverse direction of an egress flow is carried by ct state
# and nothing else. So if ct state is lost, a live connection becomes a dropped
# one — case D is the sharp form of the question, and B/C are the two mechanisms
# an Adopt implementation would actually execute.
#
# ── Why each case exists ──────────────────────────────────────────────────────
#
# (a)  Baseline. Establishes that the fixture measures a REAL flow: a long-lived
#      guest→peer TCP that keeps transferring, asserted by BYTES MOVING (the
#      peer's per-message RX counter grows), not by a socket merely existing.
#      A "still alive" claim read off a socket would pass on a dead flow.
# (a2) Paired negative: a floor whose drop
#      counter is 0 is indistinguishable from a floor that is bypassed. So the
#      same rule set must be shown to DENY something (guest→peer on an
#      undeclared port) with the drop counter moving. Without (a2), every
#      "survived" verdict below could be a verdict about a floor that is not there.
# (b)  Mechanism B above. Atomic replace under load.
# (c)  Mechanism C above. Delete, observe the gap, reinstall. B and C are
#      DIFFERENT mechanisms (one never has a window without rules, the other
#      does) and are reported separately. Attribution matters as much as the
#      verdict here, so the PEER netns carries its own counters splitting
#      arrivals by source address: a properly-SNAT'd packet vs one that arrived
#      with the raw sandbox address. Without that split, "the flow stalled in the
#      gap" cannot be told apart from "the NAT transform went missing".
# (c2) Discriminator for (c): the same delete/gap/reinstall on a FILTER-ONLY table
#      (no nat chain at all, hence no NAT transform in play). A single `delete
#      table` removes the floor AND the nat hook simultaneously, so if (c) ever
#      breaks a flow, (c) alone cannot say which half did it. (c2) removes only
#      the floor half. Both currently survive, and (c2) stays precisely so a
#      future regression in (c) is attributable rather than merely observed.
# (d)  Mechanism D above. Flush ct state under load.
# (d2) `nf_conntrack_tcp_loose` is 1 on this host, which makes conntrack ADOPT a
#      mid-stream flow it has no entry for. That sysctl therefore decides case
#      (d)'s outcome, so (d) is re-run with it off — otherwise the (d) number is
#      a fact about one sysctl value, silently reported as a fact about Adopt.
#
# EVERY case builds its OWN fresh guest + flow. The first draft of this spike
# chained them onto one long-lived flow, and case (c) killed it — so case (d) was
# measuring a corpse and reported "died on flush" for the wrong reason. That
# reading was caught only because the paired counter assertion refused to
# attribute it (no floor counter moved). Per-case flows are the fix, and the
# reason is recorded here so nobody re-chains them.
# (e)  Which side carries the flow. The guest end lives in the sentry's netstack
#      (`--network=sandbox`), and prior hook measurements showed that guest packets do not
#      traverse the sandbox netns's L3 hooks. So the assertion is: the HOST ct
#      table holds an entry keyed by the guest IP, and the sandbox netns's own ct
#      table holds nothing.
#
# ── Fixture ───────────────────────────────────────────────────────────────────
#
# A real runsc guest (--network=sandbox, the runtime provider contract pinned argv) doing EGRESS to a
# peer in a second netns, with the three host mechanisms required for egress:
# necessary and sufficient: ip_forward=1, a guest-side default route WITH `dev`
# installed BEFORE `runsc run` (the sentry takes the L3 address off the
# kernel-side veth at boot, so afterwards `ip route add` reports
# `Nexthop has invalid gateway`), and host-side masquerade for that /30.
#
# Egress-with-masquerade is the right shape and not an arbitrary one: it makes ct
# state load-bearing TWICE over, so a ct loss has somewhere to show up.
#   - the floor's `established,related` arm is the only thing that accepts the
#     reverse direction (the `iifname <cveth> drop` arm below closes the
#     `policy accept` fallthrough that would otherwise make this arm decorative);
#   - the reverse packet cannot even be routed back to the guest without the ct
#     entry's stored NAT transform.
# `post_masq` is the instrument for the second one: the nat hook only evaluates
# for the FIRST packet of a ct entry, so post_masq moving == a NEW ct entry was
# created, and post_masq frozen while data flows == the flow is riding the OLD
# entry's transform.
#
# ctprobe's source is generated into the work dir rather than added to the repo:
# the measurement needs a streaming echo/stream pair (the existing pubprobe does
# one banner and closes, and the spike rootfs has no coreutils/nc), and this
# spike is the only consumer.
#
# Usage: sudo ./spike-ct-adopt.sh   (needs root: runsc, netns, nft, conntrack)
set -euo pipefail

WORK="$(mktemp -d /tmp/spike-ct.XXXXXX)"
ROOTFS="$WORK/rootfs"; BUNDLE="$WORK/bundle"; RUNSC_ROOT="$WORK/runsc-root"
CID="spikect$$"
TABLE="spikect$$"

# Sandbox side: the /30 shape local_runsc uses today within 10.66.0.0/16.
GNS="spikect-g$$"; GVETH_H="sctgh$$"; GVETH_G="sctgg$$"
GHOST_IP="10.66.9.1"; GUEST_IP="10.66.9.2"; SBX_CIDR="10.66.9.0/30"
# Peer side: a distinct /30 standing in for "something the host routes to".
CNS="spikect-c$$"; CVETH_H="sctch$$"; CVETH_C="sctcc$$"
CHOST_IP="10.77.9.1"; PEER_IP="10.77.9.2"

ECHO_PORT=9000        # declared in the floor: the flow under measurement
DENIED_PORT=9999      # NOT declared: the (a2) negative control
PEERLOG="$WORK/peer.log"
LOG="$WORK/guest.log"
PEER_PID=""
PTABLE="spikectp$$"   # peer-side observation table (attribution, not enforcement)
LOOSE_ORIG="$(cat /proc/sys/net/netfilter/nf_conntrack_tcp_loose 2>/dev/null || echo 1)"

pass=0; fail=0; invalid=0
ok()   { echo "  PASS: $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $*"; fail=$((fail+1)); }
inv()  { echo "  INVALID: $*"; invalid=$((invalid+1)); }
note() { echo "  note: $*"; }

cleanup() {
  [[ -n "$PEER_PID" ]] && kill "$PEER_PID" 2>/dev/null || true
  runsc --root="$RUNSC_ROOT" delete --force "$CID" 2>/dev/null || true
  nft delete table inet "$TABLE" 2>/dev/null || true
  ip netns exec "$CNS" nft delete table inet "$PTABLE" 2>/dev/null || true
  ip netns del "$GNS" 2>/dev/null || true
  ip netns del "$CNS" 2>/dev/null || true
  ip link del "$GVETH_H" 2>/dev/null || true
  ip link del "$CVETH_H" 2>/dev/null || true
  sysctl -qw net.netfilter.nf_conntrack_tcp_loose="$LOOSE_ORIG" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== substrate facts this measurement depends on =="
note "conntrack: $(conntrack --version 2>&1 | head -1)"
note "/proc/net/nf_conntrack: $([[ -e /proc/net/nf_conntrack ]] && echo present || echo 'ABSENT (CONFIG_NF_CONNTRACK_PROCFS unset)')"
note "ip_forward=$(cat /proc/sys/net/ipv4/ip_forward) tcp_loose=$LOOSE_ORIG ct_count=$(conntrack -C)"
note "runsc: $(runsc --version 2>&1 | head -1)"

# ── ctprobe ───────────────────────────────────────────────────────────────────
mkdir -p "$WORK/ctprobe"
cat > "$WORK/ctprobe/main.go" <<'GOEOF'
// ctprobe — the streaming probe spike-ct-adopt.sh measures with.
//
//	ctprobe echo   <bind>                      TCP echo server; prints RX=<n> per
//	                                           message, so the HOST side has a
//	                                           byte-movement counter that does not
//	                                           depend on the guest's log flushing.
//	ctprobe stream <addr> <total-ms> <iv-ms>   dial once, then write/read a
//	                                           sequence forever; prints SEQ_OK,
//	                                           SEQ_TIMEOUT, or STREAM_BROKEN.
//	ctprobe dial   <addr> <timeout-ms>         one dial, classified (the negative
//	                                           control; same classification as
//	                                           pubprobe — refused/reset/timeout
//	                                           are distinct answers and shell
//	                                           tools collapse them).
//
// A read timeout is deliberately NOT fatal: a rule-set gap may stall the flow and
// then let it resume, and collapsing stall into death would lose that.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var rx uint64

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: ctprobe echo <bind> | stream <addr> <total-ms> <iv-ms> | dial <addr> <ms>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "echo":
		echo(os.Args[2])
	case "stream":
		stream(os.Args[2], ms(os.Args[3]), ms(os.Args[4]))
	case "dial":
		dial(os.Args[2], ms(os.Args[3]))
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
}

func ms(s string) time.Duration {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad duration %q\n", s)
		os.Exit(2)
	}
	return time.Duration(n) * time.Millisecond
}

func echo(bind string) {
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Printf("ECHO_BIND_FAILED=%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("ECHO_LISTENING=%s\n", ln.Addr().String())
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					fmt.Printf("ECHO_CONN_END RX=%d ERR=%q\n", atomic.LoadUint64(&rx), err.Error())
					return
				}
				n := atomic.AddUint64(&rx, 1)
				if _, err := io.WriteString(c, line); err != nil {
					fmt.Printf("ECHO_WRITE_ERR RX=%d ERR=%q\n", n, err.Error())
					return
				}
				fmt.Printf("RX=%d MSG=%s\n", n, strings.TrimSpace(line))
			}
		}(c)
	}
}

func stream(addr string, total, iv time.Duration) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("STREAM_DIAL_FAILED=%s ERR=%q\n", classify(err), err.Error())
		os.Exit(1)
	}
	defer c.Close()
	fmt.Printf("STREAM_CONNECTED=%s REMOTE=%s\n", c.LocalAddr().String(), c.RemoteAddr().String())
	br := bufio.NewReader(c)
	deadline := time.Now().Add(total)
	seq, consec := 0, 0
	for time.Now().Before(deadline) {
		seq++
		c.SetWriteDeadline(time.Now().Add(1500 * time.Millisecond))
		if _, err := fmt.Fprintf(c, "seq %d\n", seq); err != nil {
			fmt.Printf("STREAM_BROKEN=%d PHASE=write ERR=%q\n", seq, err.Error())
			os.Exit(3)
		}
		c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		line, err := br.ReadString('\n')
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				consec++
				fmt.Printf("SEQ_TIMEOUT=%d CONSEC=%d\n", seq, consec)
				if consec >= 10 {
					fmt.Printf("STREAM_BROKEN=%d PHASE=read_stall ERR=%q\n", seq, err.Error())
					os.Exit(3)
				}
				continue
			}
			fmt.Printf("STREAM_BROKEN=%d PHASE=read ERR=%q\n", seq, err.Error())
			os.Exit(3)
		}
		consec = 0
		fmt.Printf("SEQ_OK=%d ECHO=%q\n", seq, strings.TrimSpace(line))
		time.Sleep(iv)
	}
	fmt.Printf("STREAM_DONE=%d\n", seq)
}

func dial(addr string, timeout time.Duration) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", addr, timeout)
	el := time.Since(start)
	if err != nil {
		fmt.Printf("OUTCOME=%s ELAPSED_MS=%d ERR=%q\n", classify(err), el.Milliseconds(), err.Error())
		return
	}
	c.Close()
	fmt.Printf("OUTCOME=connected ELAPSED_MS=%d\n", el.Milliseconds())
}

func classify(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host_unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "net_unreachable"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "prohibited"
	}
	return "other"
}
GOEOF

echo
echo "== build ctprobe (static) + minimal rootfs =="
( cd "$WORK/ctprobe" && GOFLAGS=-mod=mod go mod init ctprobe >/dev/null 2>&1 || true
  CGO_ENABLED=0 go build -o "$WORK/ctprobe.bin" . )
"$(cd "$(dirname "$0")" && pwd)/build-rootfs.sh" "$ROOTFS" "$WORK/ctprobe.bin" >/dev/null
cp "$WORK/ctprobe.bin" "$ROOTFS/usr/bin/ctprobe"

# ── peer netns + echo server ──────────────────────────────────────────────────
echo "== peer netns (the far end of the egress flow) =="
ip netns add "$CNS"
ip link add "$CVETH_H" type veth peer name "$CVETH_C"
ip link set "$CVETH_C" netns "$CNS"
ip addr add "$CHOST_IP/30" dev "$CVETH_H"; ip link set "$CVETH_H" up
ip netns exec "$CNS" ip addr add "$PEER_IP/30" dev "$CVETH_C"
ip netns exec "$CNS" ip link set "$CVETH_C" up
ip netns exec "$CNS" ip link set lo up
ip netns exec "$CNS" ip route add default via "$CHOST_IP" dev "$CVETH_C"

# Peer-side OBSERVATION table (policy accept throughout — it enforces nothing,
# it only splits arrivals by source so a death can be attributed):
#   p_snat = arrived with the masqueraded source (the host gateway) — correct
#   p_raw  = arrived with the RAW sandbox source, i.e. the nat transform was
#            missing, which is the signature of "the nat chain was gone", NOT of
#            "ct state was lost". These two hypotheses are otherwise identical
#            from the guest's side (both just stall).
ip netns exec "$CNS" nft -f - <<EOF
table inet $PTABLE {
  counter p_snat {}
  counter p_raw {}
  counter p_rst {}
  chain input {
    type filter hook input priority filter; policy accept;
    ip saddr $CHOST_IP tcp dport $ECHO_PORT counter name p_snat
    ip saddr $SBX_CIDR tcp dport $ECHO_PORT counter name p_raw
  }
  chain output {
    type filter hook output priority filter; policy accept;
    tcp flags & rst == rst counter name p_rst
  }
}
EOF

: > "$PEERLOG"
ip netns exec "$CNS" "$WORK/ctprobe.bin" echo "0.0.0.0:$ECHO_PORT" >>"$PEERLOG" 2>&1 &
PEER_PID=$!
for _ in $(seq 1 50); do grep -q ECHO_LISTENING "$PEERLOG" && break; sleep 0.1; done
grep -q ECHO_LISTENING "$PEERLOG" || { echo "peer echo server never came up"; exit 1; }
note "peer: $(grep -m1 ECHO_LISTENING "$PEERLOG")"

# ── rule set ──────────────────────────────────────────────────────────────────
# install_table  — the idempotent bare-`table` + `delete` + define triple in ONE
# `nft -f` file (a bare `delete table` on a missing table is rc=1 and
# aborts the whole file). This is exactly the shape an Adopt would re-apply, so
# case (b) uses it verbatim rather than a hand-rolled variant.
#
# Counters are per hook and per arm, because "it works" is not an assertion
# without a counter): every claim below is paired with the counter that justifies it.
#   post_masq  nat hook, so it counts NEW ct entries, not packets
#   fw_est     reverse direction accepted by `established,related` — the arm
#              shown below to be load-bearing
#   fw_new     the declared egress flow's opening packet
#   fw_gdrop   floor drop, guest side (the (a2) control's counter)
#   fw_rdrop   floor drop, peer side — makes fw_est the ONLY way in
install_table() {
  nft -f - <<EOF
table inet $TABLE
delete table inet $TABLE
table inet $TABLE {
  counter post_masq {}
  counter fw_est {}
  counter fw_new {}
  counter fw_gdrop {}
  counter fw_rdrop {}
  counter in_est {}
  counter in_drop {}
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    oifname "$CVETH_H" ip saddr $SBX_CIDR counter name post_masq masquerade
  }
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "$GVETH_H" ct state established,related counter name in_est accept
    iifname "$GVETH_H" counter name in_drop drop
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    ct state established,related counter name fw_est accept
    iifname "$GVETH_H" ip daddr $PEER_IP tcp dport $ECHO_PORT ct state new \
      counter name fw_new accept
    iifname "$GVETH_H" counter name fw_gdrop drop
    iifname "$CVETH_H" counter name fw_rdrop drop
  }
}
EOF
}

# install_filter_only — the (c2) control: same floor, NO nat chain, so no NAT
# transform is in play and the peer is reached on the raw sandbox address (it has
# a default route back via the host, so this routes fine). Deleting THIS table
# removes only the floor, never a nat hook.
install_filter_only() {
  nft -f - <<EOF
table inet $TABLE
delete table inet $TABLE
table inet $TABLE {
  counter post_masq {}
  counter fw_est {}
  counter fw_new {}
  counter fw_gdrop {}
  counter fw_rdrop {}
  counter in_est {}
  counter in_drop {}
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "$GVETH_H" ct state established,related counter name in_est accept
    iifname "$GVETH_H" counter name in_drop drop
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    ct state established,related counter name fw_est accept
    iifname "$GVETH_H" ip daddr $PEER_IP tcp dport $ECHO_PORT ct state new \
      counter name fw_new accept
    iifname "$GVETH_H" counter name fw_gdrop drop
    iifname "$CVETH_H" counter name fw_rdrop drop
  }
}
EOF
}

jctr() { python3 -c 'import sys,json
d=json.load(sys.stdin)
want=sys.argv[1]
for o in d["nftables"]:
    c=o.get("counter")
    if c and c.get("name")==want: print(c["packets"]); break
else: print("NA")' "$1"; }

ctr()  { nft -j list counters table inet "$TABLE" 2>/dev/null | jctr "$1"; }
pctr() { ip netns exec "$CNS" nft -j list counters table inet "$PTABLE" 2>/dev/null | jctr "$1"; }

counters() { echo "post_masq=$(ctr post_masq) fw_new=$(ctr fw_new) fw_est=$(ctr fw_est)" \
                  "fw_gdrop=$(ctr fw_gdrop) fw_rdrop=$(ctr fw_rdrop)" \
                  "in_est=$(ctr in_est) in_drop=$(ctr in_drop)"; }
pcounters() { echo "p_snat=$(pctr p_snat) p_raw=$(pctr p_raw) p_rst=$(pctr p_rst)"; }

# ── guest ─────────────────────────────────────────────────────────────────────
# make_netns: netns + veth + guest-side default route. The route MUST go in here,
# before `runsc run` — after boot the sentry has taken the L3
# address off the kernel-side veth and `ip route add` fails with
# `Nexthop has invalid gateway`.
make_netns() {
  runsc --root="$RUNSC_ROOT" delete --force "$CID" 2>/dev/null || true
  ip netns del "$GNS" 2>/dev/null || true
  ip link del "$GVETH_H" 2>/dev/null || true
  ip netns add "$GNS"
  ip link add "$GVETH_H" type veth peer name "$GVETH_G"
  ip link set "$GVETH_G" netns "$GNS"
  ip addr add "$GHOST_IP/30" dev "$GVETH_H"; ip link set "$GVETH_H" up
  ip netns exec "$GNS" ip addr add "$GUEST_IP/30" dev "$GVETH_G"
  ip netns exec "$GNS" ip link set "$GVETH_G" up
  ip netns exec "$GNS" ip link set lo up
  ip netns exec "$GNS" ip route add default via "$GHOST_IP" dev "$GVETH_G"
}

# run_guest <total-ms> — launch the guest streaming to the peer, wait until the
# flow is actually established (STREAM_CONNECTED), not merely until runsc returns.
run_guest() {
  local total="$1"
  mkdir -p "$BUNDLE"
  cat > "$BUNDLE/config.json" <<EOF
{
  "ociVersion": "1.0.0",
  "process": { "terminal": false, "user": {"uid":0,"gid":0},
    "args": ["/usr/bin/ctprobe","stream","$PEER_IP:$ECHO_PORT","$total","200"],
    "cwd": "/", "env": ["PATH=/usr/bin:/bin:/usr/sbin:/sbin"] },
  "root": { "path": "$ROOTFS", "readonly": false },
  "hostname": "spikect",
  "mounts": [
    {"destination":"/proc","type":"proc","source":"proc"},
    {"destination":"/dev","type":"tmpfs","source":"tmpfs"},
    {"destination":"/tmp","type":"tmpfs","source":"tmpfs"},
    {"destination":"/sys","type":"sysfs","source":"sysfs","options":["ro"]}
  ],
  "linux": { "namespaces": [
    {"type":"pid"},{"type":"mount"},{"type":"ipc"},{"type":"uts"},
    {"type":"network","path":"/var/run/netns/$GNS"} ] }
}
EOF
  : > "$LOG"
  # --detach with stdio on a PIPE deadlocks Cmd.Wait (memory: runsc-detach-stdio-
  # deadlock); a file is also how we read the guest's own view of the flow.
  runsc --root="$RUNSC_ROOT" --platform=systrap --network=sandbox \
    --overlay2=none --file-access=shared --systemd-cgroup=false --net-raw=false \
    run --detach --bundle="$BUNDLE" "$CID" >"$LOG" 2>&1

  local deadline=$((SECONDS+20))
  while (( SECONDS < deadline )); do
    grep -q "STREAM_CONNECTED\|STREAM_DIAL_FAILED" "$LOG" && break
    sleep 0.2
  done
  if ! grep -q STREAM_CONNECTED "$LOG"; then
    echo "  !! guest flow never established; log:" >&2
    sed 's/^/     /' "$LOG" >&2
    return 1
  fi
  GUEST_SPORT="$(sed -n 's/.*STREAM_CONNECTED=[0-9.]*:\([0-9]*\) .*/\1/p' "$LOG" | head -1)"
  note "guest flow: $(grep -m1 STREAM_CONNECTED "$LOG")  sport=$GUEST_SPORT"
}

# ── observation helpers ───────────────────────────────────────────────────────
rx()      { grep -o 'RX=[0-9]*' "$PEERLOG" | tail -1 | cut -d= -f2; }
ct_line() { conntrack -L -p tcp -s "$GUEST_IP" --sport "$GUEST_SPORT" -o id 2>/dev/null | head -1; }
ct_id()   { sed -n 's/.*id=\([0-9]*\).*/\1/p' <<<"$(ct_line)"; }
ct_state(){ awk '{print $4}' <<<"$(ct_line)"; }
ct_of()   { local l; l="$(ct_line)"; if [[ -z "$l" ]]; then echo "ABSENT"; else
              echo "state=$(awk '{print $4}' <<<"$l") id=$(sed -n 's/.*id=\([0-9]*\).*/\1/p' <<<"$l")"; fi; }
guest_broken() { grep -q STREAM_BROKEN "$LOG" && grep -m1 -o 'STREAM_BROKEN=.*' "$LOG" || echo ""; }

# moved <before> <window-s> <label> — the byte-movement assertion. Returns 0 if
# the peer echoed at least one more message during the window.
moved() {
  local before="$1" win="$2" label="$3" after
  sleep "$win"
  after="$(rx)"
  if (( after > before )); then
    note "$label: bytes MOVED (peer RX $before → $after over ${win}s)"
    return 0
  fi
  note "$label: bytes FROZEN (peer RX stuck at $after over ${win}s)"
  return 1
}

# ── CASE A: baseline ──────────────────────────────────────────────────────────
echo
echo "===== CASE A: baseline — a live egress flow through the floor ====="
make_netns
install_table
run_guest 90000
rxA="$(rx)"
if moved "$rxA" 4 "A"; then
  ok "A flow is real and transferring; $(counters)"
else
  inv "A flow never transferred — everything below would be measuring a dead flow"
  echo "  guest log:"; sed 's/^/     /' "$LOG" | tail -20
  exit 1
fi
estA="$(ctr fw_est)"
sleep 2
estA2="$(ctr fw_est)"
(( estA2 > estA )) \
  && ok "A the \`established,related\` arm is carrying the reverse direction (fw_est $estA → $estA2)" \
  || bad "A fw_est did NOT grow ($estA → $estA2) — the arm is not on the path, so a"
(( estA2 > estA )) || bad "    ct-loss verdict below would be about nothing"
note "A ct entry: $(ct_of)"
note "A raw ct:   $(ct_line)"

echo
echo "-- A2: paired negative — the floor must DENY an undeclared port, counter moving --"
gdropA="$(ctr fw_gdrop)"
rA2="$(runsc --root="$RUNSC_ROOT" exec "$CID" /usr/bin/ctprobe dial "$PEER_IP:$DENIED_PORT" 2500 2>&1 | tail -1)"
gdropA2="$(ctr fw_gdrop)"
echo "  guest→peer:$DENIED_PORT : $rA2"
outA2="$(sed -n 's/.*OUTCOME=\([a-z_]*\).*/\1/p' <<<"$rA2")"
if [[ "$outA2" != connected && "$gdropA2" -gt "$gdropA" ]]; then
  ok "A2 unreachable ($outA2) AND floor counter moved (fw_gdrop $gdropA → $gdropA2)"
  ok "    ⟹ the floor under test is real, not bypassed"
elif [[ "$outA2" != connected ]]; then
  inv "A2 unreachable ($outA2) but fw_gdrop did NOT move ($gdropA → $gdropA2) — per"
  inv "    this is an invalid assertion: the floor may be bypassed entirely"
else
  bad "A2 the undeclared port was REACHABLE — the floor is not enforcing"
fi

# ── CASE B: atomic replace ────────────────────────────────────────────────────
echo
echo "===== CASE B: atomic rule-set REPLACE under load (the Adopt shape) ====="
# Repeated for the same reason as C: B is the case whose verdict Adopt depends on,
# and a single "survived" is exactly the reading that misled about C. If B has a
# bad side at all, repetition is how it shows.
B_TRIALS=5
bSurv=0; bDied=0; bSameID=0; bNoReNAT=0; bEstUp=0; bInvalid=0
for t in $(seq 1 $B_TRIALS); do
  echo "  -- B trial $t/$B_TRIALS --"
  make_netns; install_table; run_guest 40000
  if ! moved "$(rx)" 2 "B$t-pre"; then
    inv "B$t flow never transferred before the replace — trial invalid"
    bInvalid=$((bInvalid+1)); continue
  fi
  idB0="$(ct_id)"; rxB0="$(rx)"; prawB0="$(pctr p_raw)"
  note "B$t before: ct $(ct_of) ; peer RX=$rxB0 ; $(counters)"
  install_table                     # the same idempotent triple, atomically
  idB1="$(ct_id)"
  if moved "$rxB0" 3 "B$t"; then
    note "B$t SURVIVED the atomic replace"; bSurv=$((bSurv+1))
  else
    note "B$t DIED across the atomic replace: $(guest_broken)"; bDied=$((bDied+1))
  fi
  [[ -n "$idB0" && "$idB0" == "$idB1" ]] && bSameID=$((bSameID+1))
  [[ "$(ctr post_masq)" == "0" ]] && bNoReNAT=$((bNoReNAT+1))
  (( $(ctr fw_est) > 0 )) && bEstUp=$((bEstUp+1))
  note "B$t after: ct $(ct_of) ; $(counters) ; p_raw +$(( $(pctr p_raw) - prawB0 ))"
done
echo
if (( bDied == 0 && bSurv > 0 )); then
  ok "B the flow SURVIVED an atomic rule-set replace in $bSurv/$B_TRIALS trials, 0 deaths"
else
  bad "B the flow DIED in $bDied/$B_TRIALS trials of an atomic replace"
fi
(( bSameID == bSurv + bDied )) \
  && ok "B the SAME ct entry persisted across the replace in every trial (id unchanged)" \
  || bad "B the ct entry changed identity in $(( bSurv + bDied - bSameID )) trial(s)"
(( bNoReNAT == bSurv + bDied )) \
  && ok "B post_masq=0 in every trial while data flowed ⟹ no re-NAT: the flow rides the" \
  && ok "    OLD entry's stored transform, so the nat hook is never re-consulted" \
  || note "B post_masq moved in $(( bSurv + bDied - bNoReNAT )) trial(s) — something re-NAT'd"
(( bEstUp == bSurv + bDied )) \
  && ok "B the fresh table's \`established\` arm picked the live flow up in every trial" \
  || bad "B the fresh table's established arm showed 0 in $(( bSurv + bDied - bEstUp )) trial(s)"
(( bInvalid == 0 )) || note "B had $bInvalid invalid trial(s), excluded"

# ── CASE C: delete, gap, reinstall ────────────────────────────────────────────
echo
echo "===== CASE C: rule-set DELETE, an observable GAP, then reinstall ====="
# C IS RUN N TIMES ON PURPOSE. The first version of this case ran once and
# reported "survived"; a rerun of the identical script reported "died". It is a
# RACE, so a single trial reports whichever side of it that run landed on, and
# the honest deliverable is a RATE plus the mechanism. Each trial gets its own
# flow, and the peer-side p_raw/p_rst counters attribute each death.
C_TRIALS=5
cSurv=0; cDied=0; cRaw=0; cInvalid=0
for t in $(seq 1 $C_TRIALS); do
  echo "  -- C trial $t/$C_TRIALS --"
  make_netns; install_table; run_guest 40000
  if ! moved "$(rx)" 2 "C$t-pre"; then
    inv "C$t flow never transferred before the delete — trial invalid"
    cInvalid=$((cInvalid+1)); continue
  fi
  idC0="$(ct_id)"; rxC0="$(rx)"; psnatC0="$(pctr p_snat)"; prawC0="$(pctr p_raw)"
  prstC0="$(pctr p_rst)"
  nft delete table inet "$TABLE"
  nft list table inet "$TABLE" >/dev/null 2>&1 \
    && bad "C$t table still present after delete — the gap did not happen"
  moved "$rxC0" 2 "C$t-gap" || true
  idCgap="$(ct_id)"
  [[ -n "$idCgap" && "$idCgap" == "$idC0" ]] \
    && note "C$t ct entry survived the gap unchanged (id=$idC0, state=$(ct_state))" \
    || bad "C$t ct entry did NOT survive the gap (id $idC0 → ${idCgap:-ABSENT})"
  dRaw=$(( $(pctr p_raw) - prawC0 )); dRst=$(( $(pctr p_rst) - prstC0 ))
  note "C$t gap deltas: p_snat +$(( $(pctr p_snat) - psnatC0 )) p_raw +$dRaw p_rst +$dRst"
  rxC1="$(rx)"
  install_table
  if moved "$rxC1" 3 "C$t-after"; then
    note "C$t SURVIVED delete+gap+reinstall"
    cSurv=$((cSurv+1))
  else
    note "C$t DIED: $(guest_broken)"
    cDied=$((cDied+1))
    # Attribution: the ct entry is unchanged above, so ct LOSS is not the cause.
    # A p_raw delta means a guest packet reached the peer with its RAW source —
    # the stored NAT transform was not applied for that packet — and the peer's
    # RST is then the correct reply, since that tuple is not a connection it knows.
    if (( dRaw > 0 )); then
      note "C$t ATTRIBUTED: p_raw +$dRaw (raw-source packet reached the peer) and p_rst +$dRst"
      note "    ⟹ cause is a MISSED NAT TRANSFORM during the gap, NOT lost ct state"
      cRaw=$((cRaw+1))
    else
      inv "C$t died with no p_raw delta — unattributed by these instruments"
    fi
  fi
done
echo
if (( cDied == 0 )); then
  ok "C survived $cSurv/$C_TRIALS trials — no death observed in this run, but see the"
  ok "    trial-count comment: a previous run of this identical script DID kill the flow,"
  ok "    so treat C as a race whose bad side is reachable, not as a clean pass"
elif (( cSurv == 0 )); then
  bad "C died in ALL $cDied/$C_TRIALS trials — a non-atomic delete+reinstall reliably"
  bad "    kills a live flow"
else
  bad "C is a RACE: survived $cSurv/$C_TRIALS, died $cDied/$C_TRIALS"
  bad "    ($cRaw of the deaths attributed to a raw-source packet escaping during the gap)"
  bad "    ⟹ a non-atomic delete+reinstall can kill a live flow; the atomic replace (B) cannot"
fi
(( cInvalid == 0 )) || note "C had $cInvalid invalid trial(s), excluded from the rate"

echo
echo "-- C-fast: delete + reinstall back-to-back, NO observable gap --"
# The decision-relevant refinement. An implementer reading C will object that
# their Adopt does `delete` then `install` milliseconds apart, not seconds. This
# measures exactly that: same two commands, nothing between them. If it still
# kills the flow, then "make the gap small" is not a mitigation and the only
# answer is the single atomic `-f` file (case B).
CF_TRIALS=3
cfSurv=0; cfDied=0; cfInvalid=0
for t in $(seq 1 $CF_TRIALS); do
  echo "  -- C-fast trial $t/$CF_TRIALS --"
  make_netns; install_table; run_guest 40000
  if ! moved "$(rx)" 2 "CF$t-pre"; then
    inv "CF$t flow never transferred — trial invalid"; cfInvalid=$((cfInvalid+1)); continue
  fi
  rxCF0="$(rx)"; prawCF0="$(pctr p_raw)"; prstCF0="$(pctr p_rst)"; idCF0="$(ct_id)"
  nft delete table inet "$TABLE"; install_table     # back-to-back, no sleep
  note "CF$t delete+install issued back-to-back"
  if moved "$rxCF0" 3 "CF$t"; then
    note "CF$t SURVIVED a back-to-back delete+install"; cfSurv=$((cfSurv+1))
  else
    note "CF$t DIED: $(guest_broken)"; cfDied=$((cfDied+1))
  fi
  note "CF$t deltas: p_raw +$(( $(pctr p_raw) - prawCF0 )) p_rst +$(( $(pctr p_rst) - prstCF0 ))" \
       "; ct $(ct_of) (was id=$idCF0)"
done
echo
if (( cfDied > 0 )); then
  bad "C-fast: even a back-to-back delete+install killed the flow in $cfDied/$CF_TRIALS trials"
  bad "    ⟹ shrinking the gap is NOT a mitigation. Only the single atomic \`-f\` file (B) is."
else
  ok "C-fast survived $cfSurv/$CF_TRIALS — a back-to-back delete+install did not kill the flow,"
  ok "    so C's deaths need a gap long enough for the flow to send during it. This is a"
  ok "    RACE on packet timing, not a guarantee: prefer B's atomic form regardless."
fi
(( cfInvalid == 0 )) || note "C-fast had $cfInvalid invalid trial(s), excluded"

echo
echo "-- C2: control — same delete/gap/reinstall on a FILTER-ONLY table (no nat) --"
make_netns; install_filter_only; run_guest 60000
if ! moved "$(rx)" 3 "C2-pre"; then
  inv "C2 flow never transferred (filter-only, raw address) — invalid measurement"
else
  idC20="$(ct_id)"; rxC20="$(rx)"; prstC20="$(pctr p_rst)"
  note "C2 before: ct $(ct_of) ; peer RX=$rxC20 ; $(pcounters)"
  nft delete table inet "$TABLE"
  note "C2 floor GONE (no nat chain ever existed, so ONLY the floor is missing)"
  if moved "$rxC20" 3 "C2-gap"; then
    ok "C2 the flow kept transferring during a FLOOR-ONLY gap"
  else
    note "C2 the flow stalled during the floor-only gap"
  fi
  rxC21="$(rx)"
  install_filter_only
  note "C2 reinstalled"
  if moved "$rxC21" 4 "C2-after"; then
    ok "C2 the flow SURVIVED a floor-only delete+gap+reinstall"
    ok "    ⟹ ISOLATES C's cause to the NAT half. Removing the FLOOR mid-flow is harmless"
    ok "      (the ct entry still satisfies \`established\` the moment the arm is back);"
    ok "      removing the NAT CHAIN mid-flow is what kills it, because a packet sent during"
    ok "      the gap leaves un-SNAT'd and draws a RST from the peer. Adopt's hazard is"
    ok "      therefore specifically the nat chain, not the filter chains."
  else
    bad "C2 the flow DIED across a FLOOR-ONLY gap $(guest_broken) — the floor's absence is"
    bad "    itself sufficient to kill a live flow, so C's cause is NOT isolated to NAT"
  fi
  note "C2 after: ct $(ct_of) ; $(counters) ; peer RST +$(( $(pctr p_rst) - prstC20 ))"
fi

# ── CASE D: flush ct state ────────────────────────────────────────────────────
echo
echo "===== CASE D: flush conntrack under load (tcp_loose=$LOOSE_ORIG, as found) ====="
make_netns; install_table; run_guest 60000
if ! moved "$(rx)" 3 "D-pre"; then
  inv "D flow never transferred before the flush — invalid measurement"; exit 1
fi
idD0="$(ct_id)"; rxD0="$(rx)"; masqD0="$(ctr post_masq)"; estD0="$(ctr fw_est)"
rdropD0="$(ctr fw_rdrop)"; gdropD0="$(ctr fw_gdrop)"
prawD0="$(pctr p_raw)"; psnatD0="$(pctr p_snat)"
note "D before: ct $(ct_of) ; peer RX=$rxD0 ; ct_count=$(conntrack -C) ; $(pcounters)"
conntrack -F 2>&1 | sed 's/^/     /'
note "D ct table flushed; ct_count=$(conntrack -C) ; entry now: $(ct_of)"
if moved "$rxD0" 5 "D"; then
  ok "D the flow SURVIVED a conntrack flush"
  survD=1
else
  ok "D the flow DIED on a conntrack flush — ct loss IS a live-connection killer"
  survD=0
fi
note "D after: ct $(ct_of) ; $(counters) ; $(pcounters)"
note "D deltas: post_masq +$(( $(ctr post_masq) - masqD0 )) fw_est +$(( $(ctr fw_est) - estD0 ))" \
     "fw_gdrop +$(( $(ctr fw_gdrop) - gdropD0 )) fw_rdrop +$(( $(ctr fw_rdrop) - rdropD0 ))" \
     "p_snat +$(( $(pctr p_snat) - psnatD0 )) p_raw +$(( $(pctr p_raw) - prawD0 ))"
note "D guest: $(guest_broken)"
idD1="$(ct_id)"
if (( survD == 1 )); then
  if [[ -n "$idD1" && "$idD1" != "$idD0" ]]; then
    note "D mechanism: a NEW ct entry (id $idD0 → $idD1) was created mid-stream and the"
    note "    flow rode it. That is nf_conntrack_tcp_loose=$LOOSE_ORIG picking up a flow it"
    note "    never saw open — repeated below with it OFF, since survival here is a"
    note "    fact about that sysctl, not about Adopt."
  fi
else
  # Two candidate mechanisms for a post-flush death, and they are separable:
  #   floor  — the reverse packet no longer matches `established,related` and
  #            hits a drop arm (the same mechanism as for a fresh flow, now on a LIVE flow)
  #   nat    — the guest's packet lost its transform and reached the peer raw
  if (( $(ctr fw_rdrop) > rdropD0 || $(ctr fw_gdrop) > gdropD0 )); then
    ok "D ATTRIBUTED to the FLOOR: a drop arm moved (fw_gdrop +$(( $(ctr fw_gdrop) - gdropD0 )),"
    ok "    fw_rdrop +$(( $(ctr fw_rdrop) - rdropD0 ))) ⟹ unreachable AND the counter moved"
  elif (( $(pctr p_raw) > prawD0 )); then
    ok "D ATTRIBUTED to lost NAT: peer saw p_raw +$(( $(pctr p_raw) - prawD0 )) raw-source packets"
    ok "    ⟹ the flush destroyed the stored transform, so the flow was unroutable back"
  else
    inv "D the flow died but no floor counter and no peer counter moved — cause"
    inv "    unattributed by these instruments; do NOT read this as a floor verdict"
  fi
fi

# ── CASE conntrack-loose-off: same flush with tcp_loose off ───────────────────
echo
echo "===== CASE conntrack-loose-off: conntrack flush with nf_conntrack_tcp_loose=0 ====="
make_netns
install_table
run_guest 60000
rxD20="$(rx)"
if ! moved "$rxD20" 3 "conntrack-loose-off-pre"; then
  inv "conntrack-loose-off flow never transferred before the flush — invalid measurement"
else
  sysctl -qw net.netfilter.nf_conntrack_tcp_loose=0
  note "conntrack-loose-off tcp_loose=$(cat /proc/sys/net/netfilter/nf_conntrack_tcp_loose)"
  idD20="$(ct_id)"; rdropD20="$(ctr fw_rdrop)"; gdropD20="$(ctr fw_gdrop)"
  masqD20="$(ctr post_masq)"; rxD21="$(rx)"; prawD20="$(pctr p_raw)"
  note "conntrack-loose-off before: ct $(ct_of) ; peer RX=$rxD21 ; $(pcounters)"
  conntrack -F 2>&1 | sed 's/^/     /'
  note "conntrack-loose-off flushed; entry now: $(ct_of)"
  if moved "$rxD21" 5 "conntrack-loose-off"; then
    note "conntrack-loose-off flow SURVIVED even with tcp_loose=0"
    survD2=1
  else
    ok "conntrack-loose-off flow DIED on the flush with tcp_loose=0"
    survD2=0
  fi
  note "conntrack-loose-off after: ct $(ct_of) ; $(counters) ; $(pcounters)"
  note "conntrack-loose-off deltas: post_masq +$(( $(ctr post_masq) - masqD20 ))" \
       "fw_gdrop +$(( $(ctr fw_gdrop) - gdropD20 )) fw_rdrop +$(( $(ctr fw_rdrop) - rdropD20 ))" \
       "p_raw +$(( $(pctr p_raw) - prawD20 ))"
  note "conntrack-loose-off guest: $(guest_broken)"
  if (( survD2 == 0 )); then
    if (( $(ctr fw_gdrop) > gdropD20 || $(ctr fw_rdrop) > rdropD20 )); then
      ok "conntrack-loose-off ATTRIBUTED to the FLOOR: a drop counter moved (fw_gdrop +$(( $(ctr fw_gdrop) - gdropD20 )),"
      ok "    fw_rdrop +$(( $(ctr fw_rdrop) - rdropD20 ))) ⟹ packets dropped for want of ct state,"
      ok "    which is the floor firing on a LIVE flow rather than a fresh one"
    elif (( $(pctr p_raw) > prawD20 )); then
      ok "conntrack-loose-off ATTRIBUTED to lost NAT: p_raw +$(( $(pctr p_raw) - prawD20 )) — the transform went with the entry"
    else
      inv "conntrack-loose-off died with no floor and no peer counter movement — cause unattributed"
    fi
  fi
  sysctl -qw net.netfilter.nf_conntrack_tcp_loose="$LOOSE_ORIG"
fi

# ── CASE E: which side carries the flow ───────────────────────────────────────
echo
echo "===== CASE E: gVisor-terminated — which side holds the ct entry ====="
make_netns
install_table
run_guest 30000
rxE="$(rx)"
if ! moved "$rxE" 3 "E"; then
  inv "E flow not transferring — cannot attribute a side"
else
  hostline="$(ct_line)"
  nsct="$(ip netns exec "$GNS" conntrack -C 2>&1 || echo "ERR")"
  nsl="$(ip netns exec "$GNS" conntrack -L -p tcp 2>/dev/null | wc -l)"
  echo "  host ct:   ${hostline:-ABSENT}"
  echo "  sandbox-netns ct count: $nsct  (entries listed: $nsl)"
  if [[ -n "$hostline" ]]; then
    ok "E the HOST ct table holds the flow, keyed by the guest IP ($GUEST_IP:$GUEST_SPORT)"
  else
    bad "E no host ct entry for the flow"
  fi
  if [[ "$nsct" == "0" || "$nsl" == "0" ]]; then
    ok "E the SANDBOX netns's own ct table holds nothing (guest packets do not"
    ok "    traverse that netns's L3 hooks) ⟹ the host entry is the sole carrier"
  else
    note "E the sandbox netns ct table is NOT empty ($nsct) — read the listing above"
  fi
  note "E the guest end of the socket lives in the sentry's netstack and is NOT"
  note "    represented in any host ct table; what we observed is host-side only."
fi

# ── summary ───────────────────────────────────────────────────────────────────
echo
echo "===== SUMMARY ====="
echo "  pass=$pass fail=$fail invalid=$invalid"
echo
echo "  A  baseline live flow + established arm carrying it"
echo "  A2 paired negative (unreachable ∧ floor counter moved)"
echo "  B  atomic rule-set REPLACE under load"
echo "  C  DELETE + gap + reinstall (nat + floor)   C2 floor-only control"
echo "  D  conntrack -F, tcp_loose as found ($LOOSE_ORIG)"
echo "  conntrack-loose-off  conntrack -F, tcp_loose=0"
echo "  E  which side carries the ct entry"
echo
echo "  Every case built its OWN flow — B/C/D/E are independent, not chained."
echo
echo "  A FAIL or INVALID above is a measurement result, not a script error — read it."
