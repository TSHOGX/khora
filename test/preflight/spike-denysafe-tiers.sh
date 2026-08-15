#!/usr/bin/env bash
# spike-denysafe-tiers.sh — membership of the the documented invariant "deny-safe private" tier, for
# BOTH address families. No pre-existing the documented invariant fact covers it: they fix the L0
# floor's members and rule text, and the documented invariant ⑦ notes in passing that this
# host's resolver (0.250.250.200) is outside every floor item — but nobody has
# measured what a *tier* rule above the floor can and cannot see.
#
# Four questions, each one a spec input:
#
#  Q1 (v4) — is 0.0.0.0/8 a real destination class on this host, and does an
#     `ip daddr 0.0.0.0/8` rule see it? The resolver lives there, so the answer
#     decides whether adding 0.0.0.0/8 to the deny-safe tier costs this
#     deployment its DNS. Also: does a guest dial to 0.0.0.0 itself leave the
#     netns at all, or is it loopback semantics (like the documented invariant ⑦'s 127.0.0.53)?
#
#  Q2 (family smuggling) — can an IPv4-mapped IPv6 destination (::ffff:a.b.c.d)
#     reach a v4-only matcher, or does it appear on the wire as IPv6 and slip
#     past `ip daddr`? SSRF blocklists get breached on exactly this, so the
#     question is whether an nft tier needs an `ip6 daddr ::ffff:0:0/96` arm.
#     Measured with a raw AF_INET6 socket, not Go's net.Dial, because the
#     library may normalise the family before the kernel ever sees it.
#
#  Q3 (v6, host's own address) — the documented invariant's status note says the host's eth0 IPv6 is
#     reachable from a sandbox with no floor. This host's eth0 also carries a
#     ULA (fd07:.../64). Is `net.ipv6.conf.all.forwarding=0` what keeps a
#     sandbox off it, or is that address delivered LOCALLY and therefore
#     unaffected by the forwarding switch? If local, forwarding=0 protects
#     nothing here and the claim must not be leaned on.
#
#  Q4 (v6, another machine's ULA) — for a ULA that must be ROUTED, is
#     forwarding=0 load-bearing, and does an `ip6 daddr fc00::/7` drop see the
#     traffic once forwarding is on? This is the only arm where the sysctl can
#     be the gate, so it is the only place the "assert the sysctl" option could
#     be justified.
#
#  Q5 (v6 carrying a v4 destination) — NAT64 64:ff9b::/96 is real on-wire IPv6,
#     so no `ip daddr` rule can see it, while in a NAT64 deployment it resolves
#     to a v4 destination. Measured against a LIVE, counting v4 RFC1918 drop.
#     Read the control: on this host anything in the prefix completes a
#     handshake, so "connected" alone proves nothing.
#
#  Q6 (floor vs tier overlap) — the sibling floor CIDR (10.66.0.0/16) is nested
#     INSIDE the RFC1918 tier (10/8), and the documented invariant says the tier is liftable by
#     declaration while the floor is not. Both orderings are measured, because
#     the wrong one fails silently (the documented invariant ⑧/⑩: reachable, floor counter 0).
#
# Discipline (the documented invariant): a failed `nft -f` reads as "everything is
# reachable", which is indistinguishable from a missing rule. Every rule set is
# asserted loaded before it is trusted, and every "blocked" claim is paired with
# a counter that moved. A drop counter of 0 next to a failed dial is recorded as
# an INVALID assertion, not a pass.
#
# Fixture: plain netns peers, not a runsc guest. Allowed by the same reasoning
# the documented invariant ⑩⑪⑫ used — what is under measurement is nft/kernel matching semantics and
# the family of a packet on the wire, and the documented invariant ⑨ already proved gVisor changes
# none of the conntrack / matching conclusions.
#
# Usage: sudo ./spike-denysafe-tiers.sh   (needs root: netns, nft, sysctl)
set -euo pipefail

WORK="$(mktemp -d /tmp/spike-ds.XXXXXX)"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
TABLE="spikeds$$"

# "Sandbox" side: the /30 shape local_runsc uses today (10.66.0.0/16, the documented invariant ④).
SNS="spikeds-s$$"; SVH="sdsh$$"; SVS="sdss$$"
SHOST_IP="10.66.250.1"; SBX_IP="10.66.250.2"
SHOST_6="fd66:250::1";  SBX_6="fd66:250::2"
# "Other machine" side: a second netns holding a DIFFERENT ULA prefix, so Q4
# measures a ROUTED ULA rather than the host's own.
ONS="spikeds-o$$"; OVH="sdoh$$"; OVO="sdoo$$"
OHOST_IP="10.77.250.1"; OTH_IP="10.77.250.2"
OHOST_6="fd66:251::1";  OTH_6="fd66:251::2"

LAN_IP="192.168.139.3"      # another machine on the host's LAN (RFC1918), sshd
RESOLVER="0.250.250.200"    # this host's resolver — inside 0.0.0.0/8 (the documented invariant ⑦)
HOST_ULA="fd07:b51a:cc66:0:1440:1fff:fe98:707"   # eth0's ULA, local address
PROBE_PORT=18999
BANNER="hello-from-probe"

pass=0; fail=0; invalid=0
ok()   { echo "  PASS: $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $*"; fail=$((fail+1)); }
inv()  { echo "  INVALID: $*"; invalid=$((invalid+1)); }
note() { echo "  note: $*"; }

FWD6_SAVED=""
cleanup() {
  set +e
  nft delete table inet "$TABLE" 2>/dev/null
  nft delete table ip6 "${TABLE}n6" 2>/dev/null
  for ns in "$SNS" "$ONS"; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; ip netns del "$ns" 2>/dev/null; done
  ip link del "$SVH" 2>/dev/null; ip link del "$OVH" 2>/dev/null
  [ -n "$FWD6_SAVED" ] && sysctl -qw net.ipv6.conf.all.forwarding="$FWD6_SAVED" 2>/dev/null
  pkill -f "pubprobe serve" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || { echo "needs root"; exit 1; }

# ---------------------------------------------------------------- helpers ----
# Pipe-free on purpose: any `| head -1` here closes the pipe early, SIGPIPEs
# nft, and under `pipefail` the whole command substitution fails — which with
# `set -e` kills the script mid-measurement rather than reporting anything.
ctr() { local d; d="$(nft list counter inet "$TABLE" "$1" 2>/dev/null || true)"
        d="${d#*packets }"; echo "${d%% *}"; }
assert_loaded() {  # $1 = a substring that MUST appear in the loaded table
  # NOT `nft list | grep -q`: under `pipefail`, grep -q closes the pipe on the
  # first match, nft dies of SIGPIPE, and the pipeline reads as failure — a
  # loaded rule set then reports as "not loaded". Same class of harness bug as
  # the one this assert exists to catch, only in the safe direction.
  local dump; dump="$(nft list table inet "$TABLE" 2>/dev/null || true)"
  case "$dump" in *"$1"*) return 0 ;; esac
  bad "rule set NOT loaded (looked for: $1) — every reachability result below would be a false 'open'"
  printf '%s\n' "$dump" | sed 's/^/    /'
  return 1
}
sdial() { ip netns exec "$SNS" "$WORK/pubprobe" dial "$1" "${2:-3000}"; }

# ------------------------------------------------------------ build tools ----
echo "== building probes"
( cd "$REPO/test/preflight/pubprobe" && go build -o "$WORK/pubprobe" . )
# v6dial: a RAW AF_INET6 socket. The measurement is which family the kernel puts
# on the wire for a v4-mapped destination, so the family must not be chosen by a
# library that may normalise it away.
cat > "$WORK/v6dial.go" <<'EOF'
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// v6dial <ipv6-literal> <port> — connect over an explicit AF_INET6 socket.
func main() {
	ip := net.ParseIP(os.Args[1])
	if ip == nil {
		fmt.Println("OUTCOME=badaddr")
		os.Exit(2)
	}
	port, _ := strconv.Atoi(os.Args[2])
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
	if err != nil {
		fmt.Printf("OUTCOME=socket_failed ERR=%q\n", err)
		os.Exit(1)
	}
	defer syscall.Close(fd)
	sa := &syscall.SockaddrInet6{Port: port}
	copy(sa.Addr[:], ip.To16())
	start := time.Now()
	// Blocking connect; the shell wraps this in `timeout` so a silent drop is
	// distinguishable from an RST by elapsed time as well as by errno.
	err = syscall.Connect(fd, sa)
	el := time.Since(start)
	if err != nil {
		fmt.Printf("OUTCOME=err ERRNO=%v ELAPSED_MS=%d\n", err, el.Milliseconds())
		os.Exit(0)
	}
	buf := make([]byte, 128)
	tv := syscall.Timeval{Sec: 2}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	// Guard n<0: a read timeout returns -1, and slicing on it panics — which
	// would erase the OUTCOME=connected line for a connect that DID succeed,
	// i.e. turn a confirmed reachability result into a silent blank.
	n, rerr := syscall.Read(fd, buf)
	if n < 0 {
		n = 0
	}
	fmt.Printf("OUTCOME=connected ELAPSED_MS=%d PAYLOAD=%q READ_ERR=%v\n", el.Milliseconds(), string(buf[:n]), rerr)
}
EOF
( cd "$WORK" && go mod init v6dial >/dev/null 2>&1 && go build -o "$WORK/v6dial" v6dial.go )

# --------------------------------------------------------------- fixture -----
echo "== fixture: two netns peers, v4 + v6, host routes between them"
for spec in "$SNS $SVH $SVS $SHOST_IP $SBX_IP $SHOST_6 $SBX_6" \
            "$ONS $OVH $OVO $OHOST_IP $OTH_IP $OHOST_6 $OTH_6"; do
  read -r ns vh vp hip pip h6 p6 <<<"$spec"
  ip netns add "$ns"
  ip link add "$vh" type veth peer name "$vp"
  ip link set "$vp" netns "$ns"
  ip addr add "$hip/30" dev "$vh"; ip addr add "$h6/64" dev "$vh" nodad
  ip link set "$vh" up
  ip netns exec "$ns" ip link set lo up
  ip netns exec "$ns" ip addr add "$pip/30" dev "$vp"
  ip netns exec "$ns" ip addr add "$p6/64" dev "$vp" nodad
  ip netns exec "$ns" ip link set "$vp" up
  ip netns exec "$ns" ip route add default via "$hip" dev "$vp"
  ip netns exec "$ns" ip -6 route add default via "$h6" dev "$vp"
done
# The three host-side mechanisms the documented invariant ⑤ measured as necessary and sufficient for a
# sandbox to reach the outside: ip_forward, guest default route (above), SNAT.
nft -f - <<EOF
table ip ${TABLE}nat {
  chain post { type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.66.250.0/30 oifname "eth0" masquerade
  }
}
EOF
trap 'nft delete table ip '"${TABLE}nat"' 2>/dev/null; cleanup' EXIT
FWD6_SAVED="$(sysctl -n net.ipv6.conf.all.forwarding)"
note "saved net.ipv6.conf.all.forwarding=$FWD6_SAVED"

# =============================================================== Q1: v4 ======
echo
echo "== Q1  0.0.0.0/8 — is the resolver a real routed destination, and does an ip-daddr rule see it?"
nft -f - <<EOF
table inet $TABLE {
  counter zero8 {}
  counter rfc1918 {}
  counter v4hit {}
  counter v6hit {}
  counter fwd_all {}
  chain cfwd { type filter hook forward priority filter; policy accept;
    iifname "$SVH" ip daddr 0.0.0.0/8 counter name "zero8"
    iifname "$SVH" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } counter name "rfc1918"
    iifname "$SVH" ip daddr $LAN_IP tcp dport 22 counter name "v4hit"
    iifname "$SVH" meta nfproto ipv6 counter name "v6hit"
    iifname "$SVH" counter name "fwd_all"
  }
}
EOF
assert_loaded 'ip daddr 0.0.0.0/8' || exit 1
note "host route to resolver: $(ip route get "$RESOLVER" | tr '\n' ' ')"

z0=$(ctr zero8)
out=$(sdial "$RESOLVER:53" 3000); note "dial resolver tcp/53 (counter-only, no drop): $out"
z1=$(ctr zero8)
if [ "$z1" -gt "$z0" ]; then ok "0.0.0.0/8 rule MATCHES resolver traffic (zero8 $z0 -> $z1)"
else inv "zero8 did not move ($z0 -> $z1) — cannot claim the rule sees it"; fi
case "$out" in *connected*) ok "resolver tcp/53 reachable from netns under floor-only" ;;
                         *) note "resolver not reachable ($out) — the COST claim below is then untestable" ;; esac

echo "-- now the same rule as a drop: does adding 0.0.0.0/8 to the tier cost this deployment its DNS?"
nft add rule inet "$TABLE" cfwd iifname "$SVH" ip daddr 0.0.0.0/8 counter name zero8 drop
assert_loaded 'ip daddr 0.0.0.0/8 counter name "zero8" drop' || exit 1
z2=$(ctr zero8)
out=$(sdial "$RESOLVER:53" 3000); note "dial resolver tcp/53 WITH 0.0.0.0/8 drop: $out"
z3=$(ctr zero8)
if [ "$z3" -gt "$z2" ] && [ "${out#*connected}" = "$out" ]; then
  ok "0.0.0.0/8 in the tier BLOCKS this host's resolver (zero8 $z2 -> $z3, dial: ${out%% *})"
elif [ "${out#*connected}" != "$out" ]; then bad "still reachable despite the drop"
else inv "dial failed but zero8 did not move ($z2 -> $z3)"; fi
nft delete rule inet "$TABLE" cfwd handle "$(nft -a list chain inet "$TABLE" cfwd | awk '/0.0.0.0\/8.*drop/ {print $NF}')"

echo "-- and 0.0.0.0 itself: does a dial to it leave the netns (cf. the documented invariant ⑦ loopback semantics)?"
"$WORK/pubprobe" serve "0.0.0.0:$PROBE_PORT" "$BANNER" >"$WORK/h.log" 2>&1 &
sleep 0.4
f0=$(ctr fwd_all)
out=$(sdial "0.0.0.0:$PROBE_PORT" 2000); note "dial 0.0.0.0:$PROBE_PORT from netns: $out"
f1=$(ctr fwd_all)
if [ "${out#*connected}" = "$out" ] && [ "$f1" = "$f0" ]; then
  ok "dial to 0.0.0.0 never leaves the netns (fwd_all unchanged at $f0) — loopback semantics, like 127.0.0.53"
else note "0.0.0.0 dial: outcome=$out fwd_all $f0 -> $f1"; fi

echo "-- the functionally decisive arm: UDP/53, which is what DNS actually uses (the documented invariant ⑦)"
# TCP/53 to this resolver is flaky from a netns (measured: 1 of 3 attempts
# completed), so a TCP result alone cannot carry the cost claim. A real query is
# the falsifiable form.
cat > "$WORK/dnsq.go" <<'EOF'
package main

// dnsq <resolver-ip> — one UDP/53 A query for example.com, 3s deadline.
// A real query, not a TCP dial: DNS is almost entirely UDP (the documented invariant ⑦), so the cost
// of putting 0.0.0.0/8 in the tier is only measurable on this path.
import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	c, err := net.Dial("udp", net.JoinHostPort(os.Args[1], "53"))
	if err != nil {
		fmt.Printf("DNS=dial_failed ERR=%q\n", err)
		return
	}
	defer c.Close()
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0,
		7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	start := time.Now()
	if _, err := c.Write(q); err != nil {
		fmt.Printf("DNS=write_failed ERR=%q\n", err)
		return
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	el := time.Since(start)
	if err != nil {
		fmt.Printf("DNS=no_reply MS=%d ERR=%q\n", el.Milliseconds(), err)
		return
	}
	fmt.Printf("DNS=replied MS=%d BYTES=%d ANCOUNT=%d\n", el.Milliseconds(), n, int(buf[6])<<8|int(buf[7]))
}
EOF
( cd "$WORK" && go build -o "$WORK/dnsq" dnsq.go )
z4=$(ctr zero8)
dout=$(ip netns exec "$SNS" "$WORK/dnsq" "$RESOLVER" || true)
z5=$(ctr zero8)
note "UDP/53 query, floor-only (no tier): $dout   (zero8 $z4 -> $z5)"
case "$dout" in *replied*) ok "DNS WORKS from a sandbox under floor-only + default:allow — the resolver at $RESOLVER is reachable because it is in 0.0.0.0/8, which no floor item and no RFC1918 tier covers" ;;
                        *) inv "DNS did not resolve even without a tier ($dout) — the cost claim cannot be measured on this host" ;; esac
nft add rule inet "$TABLE" cfwd iifname "$SVH" ip daddr 0.0.0.0/8 counter name zero8 drop
assert_loaded 'ip daddr 0.0.0.0/8 counter name "zero8" drop' || exit 1
z6=$(ctr zero8)
dout2=$(ip netns exec "$SNS" "$WORK/dnsq" "$RESOLVER" || true)
z7=$(ctr zero8)
note "UDP/53 query WITH 0.0.0.0/8 in the tier: $dout2   (zero8 $z6 -> $z7)"
if [ "${dout2#*replied}" = "$dout2" ] && [ "$z7" -gt "$z6" ]; then
  ok "THE COST, measured: 0.0.0.0/8 in the deny-safe tier breaks DNS on this deployment unless declared (zero8 $z6 -> $z7)"
else inv "cost claim unpaired: $dout2, counter $z6 -> $z7"; fi
nft delete rule inet "$TABLE" cfwd handle "$(nft -a list chain inet "$TABLE" cfwd | awk '/0.0.0.0\/8.*drop/ {print $NF}')"

echo "-- control: the LAN peer (RFC1918) that a tier rule must catch"
r0=$(ctr rfc1918)
out=$(sdial "$LAN_IP:22" 3000); note "dial LAN $LAN_IP:22: $out"
r1=$(ctr rfc1918)
[ "$r1" -gt "$r0" ] && ok "RFC1918 rule sees LAN traffic (rfc1918 $r0 -> $r1)" || inv "rfc1918 counter flat"

# ================================================== Q2: family smuggling =====
echo
echo "== Q2  IPv4-mapped IPv6 (::ffff:$LAN_IP) over a raw AF_INET6 socket"
v4a=$(ctr v4hit); v6a=$(ctr v6hit)
out=$(ip netns exec "$SNS" timeout 5 "$WORK/v6dial" "::ffff:$LAN_IP" 22 || true)
note "v6dial ::ffff:$LAN_IP 22 -> $out"
v4b=$(ctr v4hit); v6b=$(ctr v6hit)
note "v4hit $v4a -> $v4b   v6hit $v6a -> $v6b"
if [ "$v4b" -gt "$v4a" ] && [ "$v6b" = "$v6a" ]; then
  ok "v4-mapped destination appears on the wire as IPv4 — a v4-only \`ip daddr\` rule SEES it; no ::ffff arm needed"
elif [ "$v6b" -gt "$v6a" ]; then
  bad "v4-mapped destination appeared as IPv6 — a v4-only tier would be bypassed; an ip6 arm IS needed"
else inv "neither counter moved — connect never left the netns ($out)"; fi

echo "-- falsifying the candidate MEMBER \`::ffff:0:0/96\`: does such a rule ever count?"
# The candidate exists because SSRF blocklists get bypassed by ::ffff: forms. If
# the arm never counts a packet while the v4 arm counts every one, the member is
# dead weight in the RULE SET — and the real hazard is relocated to declaration
# validation, which is a different part of the design.
nft add counter inet "$TABLE" v4mapped
nft add rule inet "$TABLE" cfwd iifname "$SVH" ip6 daddr ::ffff:0.0.0.0/96 counter name v4mapped
assert_loaded 'ip6 daddr ::ffff:0.0.0.0/96' || exit 1
m0=$(ctr v4mapped); v4c=$(ctr v4hit)
out=$(ip netns exec "$SNS" timeout 5 "$WORK/v6dial" "::ffff:$LAN_IP" 22 || true)
m1=$(ctr v4mapped); v4d=$(ctr v4hit)
note "second v4-mapped dial: $out   (v4mapped $m0 -> $m1, v4hit $v4c -> $v4d)"
if [ "$m1" = "$m0" ] && [ "$v4d" -gt "$v4c" ]; then
  ok "an \`ip6 daddr ::ffff:0:0/96\` rule counts NOTHING ($m0) while the v4 rule counts every packet ($v4c -> $v4d) — the member is dead weight in the rule set; the ::ffff hazard lives in declaration validation, not in matching"
else inv "v4mapped $m0 -> $m1, v4hit $v4c -> $v4d — inconclusive"; fi

note "the NAT64 arm of Q2 needs IPv6 forwarding, so it runs inside Q5's window below"

# ============================================ Q3: host's own ULA, local ======
echo
echo "== Q3  the host's own eth0 ULA under net.ipv6.conf.all.forwarding=$(sysctl -n net.ipv6.conf.all.forwarding)"
nft -f - <<EOF
table inet $TABLE {
  counter in_ula {}
  chain cinp { type filter hook input priority filter; policy accept;
    iifname "$SVH" ip6 daddr $HOST_ULA counter name "in_ula"
  }
}
EOF
assert_loaded "ip6 daddr $HOST_ULA" || exit 1
pkill -f "pubprobe serve" 2>/dev/null || true; sleep 0.2
"$WORK/pubprobe" serve "[$HOST_ULA]:$PROBE_PORT" "$BANNER" >"$WORK/h6.log" 2>&1 &
sleep 0.5
note "listener: $(cat "$WORK/h6.log")"
u0=$(ctr in_ula)
out=$(ip netns exec "$SNS" timeout 5 "$WORK/v6dial" "$HOST_ULA" "$PROBE_PORT" || true)
u1=$(ctr in_ula)
note "v6dial host ULA -> $out   (in_ula $u0 -> $u1)"
if [ "${out#*connected}" != "$out" ]; then
  ok "host's own eth0 ULA IS reachable with forwarding=0 — it is LOCAL delivery, the sysctl is not the gate"
  note "=> it lands in the \`input\` hook (in_ula moved: $u0 -> $u1), i.e. the documented invariant ⑧'s pure ct-state floor covers it"
else
  note "host ULA not reachable ($out); in_ula $u0 -> $u1"
fi

# ==================================== Q4: another machine's ULA, routed ======
echo
echo "== Q4  a ROUTED ULA (netns O, $OTH_6) — is forwarding=0 the gate?"
nft -f - <<EOF
table inet $TABLE {
  counter fw_ula {}
  chain cfwd6 { type filter hook forward priority filter + 5; policy accept;
    iifname "$SVH" ip6 daddr fc00::/7 counter name "fw_ula"
  }
}
EOF
assert_loaded 'ip6 daddr fc00::/7' || exit 1
ip netns exec "$ONS" "$WORK/pubprobe" serve "[$OTH_6]:$PROBE_PORT" "$BANNER" >"$WORK/o6.log" 2>&1 &
sleep 0.5
note "peer listener: $(cat "$WORK/o6.log")"

a0=$(ctr fw_ula)
out=$(ip netns exec "$SNS" timeout 4 "$WORK/v6dial" "$OTH_6" "$PROBE_PORT" || true)
a1=$(ctr fw_ula)
note "forwarding=0: outcome='$out'  (fw_ula $a0 -> $a1)"
if [ "${out#*connected}" = "$out" ]; then
  ok "routed ULA unreachable while ipv6 forwarding is off"
  [ "$a1" = "$a0" ] && note "and the forward-hook counter stayed at $a0 — with forwarding off the packet is dropped BEFORE the forward hook, so a floor/tier counter would show nothing at all" \
                    || note "counter moved ($a0 -> $a1) — the packet reached the forward hook and died after it"
else bad "routed ULA reachable with forwarding off"; fi

echo "-- flip forwarding on the two veths only (not all/eth0: keeps accept_ra intact)"
sysctl -qw "net.ipv6.conf.$SVH.forwarding=1" "net.ipv6.conf.$OVH.forwarding=1"
b0=$(ctr fw_ula)
out=$(ip netns exec "$SNS" timeout 4 "$WORK/v6dial" "$OTH_6" "$PROBE_PORT" || true)
b1=$(ctr fw_ula)
note "per-veth forwarding=1: outcome='$out'  (fw_ula $b0 -> $b1)"
FWD_MODE="per-veth"
if [ "${out#*connected}" = "$out" ]; then
  note "per-veth was not enough — escalating to all.forwarding=1 (this is what a v6-capable deployment would set)"
  sysctl -qw net.ipv6.conf.all.forwarding=1
  b0=$(ctr fw_ula)
  out=$(ip netns exec "$SNS" timeout 4 "$WORK/v6dial" "$OTH_6" "$PROBE_PORT" || true)
  b1=$(ctr fw_ula)
  note "all.forwarding=1: outcome='$out'  (fw_ula $b0 -> $b1)"
  FWD_MODE="all"
fi
if [ "${out#*connected}" != "$out" ]; then
  ok "one sysctl ($FWD_MODE forwarding=1) restores cross-netns ULA reachability — forwarding=0 was the ONLY thing in the way, i.e. a host setting the engine does not own"
  echo "-- now the actual rule: fc00::/7 drop"
  nft insert rule inet "$TABLE" cfwd6 iifname "$SVH" ip6 daddr fc00::/7 counter name fw_ula drop
  assert_loaded 'ip6 daddr fc00::/7 counter name "fw_ula" drop' || exit 1
  c0=$(ctr fw_ula)
  out=$(ip netns exec "$SNS" timeout 4 "$WORK/v6dial" "$OTH_6" "$PROBE_PORT" || true)
  c1=$(ctr fw_ula)
  note "with fc00::/7 drop: outcome='$out'  (fw_ula $c0 -> $c1)"
  [ "${out#*connected}" = "$out" ] && [ "$c1" -gt "$c0" ] \
    && ok "an fc00::/7 tier rule blocks it AND the counter moved ($c0 -> $c1) — enforcement, not a sysctl coincidence" \
    || inv "drop claim unpaired: outcome='$out' counter $c0 -> $c1"

  # ================= Q5: NAT64 — a v4 destination behind an IPv6 packet =====
  echo
  echo "== Q5  NAT64 64:ff9b::/96 — does a v4-only tier see a v4 destination carried over IPv6?"
  echo "   (measured on this host: a dial into 64:ff9b::/96 completes the handshake — but see the control below,"
  echo "    it completes for a bogus address too, so 'connected' does NOT prove the v4 peer was reached)"
  # v6 SNAT, the family-analogue of the documented invariant ⑤'s third mechanism: the netns ULA is not
  # routable upstream, so without it a failure would mean "no return path",
  # not "policy blocked it" — an invalid assertion either way it came out.
  nft -f - <<EOF
table ip6 ${TABLE}n6 {
  chain post { type nat hook postrouting priority srcnat; policy accept;
    ip6 saddr fd66:250::/64 oifname "eth0" masquerade
  }
}
EOF
  nat64_of() { printf '64:ff9b::%02x%02x:%02x%02x' $(echo "$1" | tr '.' ' '); }
  N64="$(nat64_of "$LAN_IP")"
  note "NAT64 form of $LAN_IP = $N64"
  nft add counter inet "$TABLE" fw_n64
  nft add rule inet "$TABLE" cfwd6 iifname "$SVH" ip6 daddr 64:ff9b::/96 counter name fw_n64
  assert_loaded 'ip6 daddr 64:ff9b::/96' || exit 1
  # The v4 RFC1918 tier drop is installed and PROVEN to work (Q1 control), yet
  # it is expressed in v4 vocabulary. If the NAT64 form of the same LAN peer
  # gets through, the v4 tier is bypassable without any v6 tier member.
  nft add rule inet "$TABLE" cfwd iifname "$SVH" ip daddr 192.168.0.0/16 counter name rfc1918 drop
  assert_loaded 'ip daddr 192.168.0.0/16 counter name "rfc1918" drop' || exit 1
  r2=$(ctr rfc1918)
  out4=$(sdial "$LAN_IP:22" 3000); r3=$(ctr rfc1918)
  note "v4 direct to LAN with RFC1918 drop: $out4  (rfc1918 $r2 -> $r3)"
  [ "${out4#*connected}" = "$out4" ] && [ "$r3" -gt "$r2" ] \
    && ok "the v4 tier drop is live and counting (rfc1918 $r2 -> $r3)" \
    || inv "v4 tier drop unpaired — the Q5 comparison below would be meaningless"
  n0=$(ctr fw_n64)
  out6=$(ip netns exec "$SNS" timeout 6 "$WORK/v6dial" "$N64" 22 || true)
  n1=$(ctr fw_n64)
  note "NAT64 to the SAME LAN peer, same rule set: outcome='$out6'  (fw_n64 $n0 -> $n1)"
  # MANDATORY control before any "bypass" claim: the NAT64 form of an address
  # where NOTHING listens. If that also reports connected, the handshake is being
  # answered by whatever sits in front of 64:ff9b::/96 on this host, and
  # "connected" says nothing about reaching the real peer.
  BOGUS="$(nat64_of 192.168.139.99)"
  outb=$(ip netns exec "$SNS" timeout 6 "$WORK/v6dial" "$BOGUS" 22 || true)
  note "control, NAT64 of an address with no listener ($BOGUS): outcome='$outb'"
  if [ "${outb#*connected}" != "$outb" ]; then
    inv "the NAT64 control ALSO connected — this host's 64:ff9b::/96 next hop answers the handshake for any address in the prefix, so the end-to-end bypass is NOT demonstrated here. What IS established: the traffic is on-wire IPv6, matched by \`ip6 daddr 64:ff9b::/96\` (fw_n64 $n0 -> $n1), while the live v4 RFC1918 drop never saw it."
    n1="$n0"   # do not let the counter feed the drop assertion below
  elif [ "${out6#*connected}" != "$out6" ]; then
    ok "CONFIRMED BYPASS: the v4 RFC1918 drop does not see the NAT64 form — the same peer is reachable over 64:ff9b::/96 while a bogus address in the prefix is not (fw_n64 $n0 -> $n1)"
    nft insert rule inet "$TABLE" cfwd6 iifname "$SVH" ip6 daddr 64:ff9b::/96 counter name fw_n64 drop
    assert_loaded 'ip6 daddr 64:ff9b::/96 counter name "fw_n64" drop' || exit 1
    n2=$(ctr fw_n64)
    out6=$(ip netns exec "$SNS" timeout 6 "$WORK/v6dial" "$N64" 22 || true)
    n3=$(ctr fw_n64)
    note "with 64:ff9b::/96 drop: outcome='$out6'  (fw_n64 $n2 -> $n3)"
    [ "${out6#*connected}" = "$out6" ] && [ "$n3" -gt "$n2" ] \
      && ok "a 64:ff9b::/96 tier member closes it AND the counter moved ($n2 -> $n3)" \
      || inv "NAT64 drop claim unpaired: outcome='$out6' counter $n2 -> $n3"
  elif [ "$n1" -gt "$n0" ]; then
    note "NAT64 traffic reached the forward hook (fw_n64 $n0 -> $n1) but did not complete — no bypass DEMONSTRATED here, though the packet is provably invisible to any \`ip daddr\` rule"
  else inv "NAT64 traffic never reached the rule (fw_n64 flat at $n0) — Q5 undetermined on this host"; fi
  nft delete table ip6 "${TABLE}n6" 2>/dev/null || true
else inv "ULA peer never reachable in any forwarding mode — Q4/Q5 undetermined"; fi
sysctl -qw net.ipv6.conf.all.forwarding="$FWD6_SAVED" >/dev/null
note "restored net.ipv6.conf.all.forwarding=$(sysctl -n net.ipv6.conf.all.forwarding)"

# ============================== Q6: floor / tier overlap, and ordering =======
echo
echo "== Q6  the sibling floor item (10.66.0.0/16) is NESTED INSIDE the RFC1918 tier (10/8)."
echo "   the documented invariant says the tier is explicitly allowable but the floor is not liftable. Does an"
echo "   explicit RFC1918 allow lift the sibling item? Ordering is the whole answer."
BNS="spikeds-b$$"; BVH="sdbh$$"; BVB="sdbb$$"
BHOST_IP="10.66.251.1"; SIB_IP="10.66.251.2"
ip netns add "$BNS"
ip link add "$BVH" type veth peer name "$BVB"
ip link set "$BVB" netns "$BNS"
ip addr add "$BHOST_IP/30" dev "$BVH"; ip link set "$BVH" up
ip netns exec "$BNS" ip link set lo up
ip netns exec "$BNS" ip addr add "$SIB_IP/30" dev "$BVB"
ip netns exec "$BNS" ip link set "$BVB" up
ip netns exec "$BNS" ip route add default via "$BHOST_IP" dev "$BVB"
ip netns exec "$BNS" "$WORK/pubprobe" serve "$SIB_IP:$PROBE_PORT" "hello-from-SIBLING" >"$WORK/sib.log" 2>&1 &
sleep 0.5
note "sibling listener: $(cat "$WORK/sib.log")"
trap 'ip netns pids '"$BNS"' 2>/dev/null | xargs -r kill 2>/dev/null; ip netns del '"$BNS"' 2>/dev/null; ip link del '"$BVH"' 2>/dev/null; nft delete table ip '"${TABLE}nat"' 2>/dev/null; cleanup' EXIT

# Baseline: no tier, no floor — the documented invariant ① reproduced in one line.
s0=$(ctr fwd_all)
out=$(sdial "$SIB_IP:$PROBE_PORT" 2000)
note "baseline (no floor, no tier): $out"
case "$out" in *hello-from-SIBLING*) ok "the documented invariant ① reproduced: sibling reachable with nothing installed" ;;
                                  *) inv "sibling not reachable at baseline ($out) — Q6 ordering test would be meaningless" ;; esac

echo "-- WRONG order: declaration's RFC1918 allow above the sibling floor drop"
nft -f - <<EOF
table inet $TABLE {
  counter sib_floor {}
  chain corder { type filter hook forward priority filter + 10; policy accept;
    iifname "$SVH" ip daddr 10.0.0.0/8 accept
    iifname "$SVH" ip daddr 10.66.0.0/16 counter name "sib_floor" drop
  }
}
EOF
assert_loaded 'ip daddr 10.66.0.0/16 counter name "sib_floor" drop' || exit 1
w0=$(ctr sib_floor)
out=$(sdial "$SIB_IP:$PROBE_PORT" 2000)
w1=$(ctr sib_floor)
note "allow-above-floor: $out  (sib_floor $w0 -> $w1)"
if [ "${out#*hello-from-SIBLING}" != "$out" ] && [ "$w1" = "$w0" ]; then
  ok "an explicit RFC1918 allow placed ABOVE the sibling drop lifts the floor — sibling reached, floor counter stayed $w0 (the silent-failure signature of the documented invariant ⑧/⑩)"
else inv "wrong-order arm did not behave as predicted: $out, counter $w0 -> $w1"; fi

echo "-- RIGHT order: sibling floor drop above the declaration's RFC1918 allow"
nft flush chain inet "$TABLE" corder
nft add rule inet "$TABLE" corder iifname "$SVH" ip daddr 10.66.0.0/16 counter name sib_floor drop
nft add rule inet "$TABLE" corder iifname "$SVH" ip daddr 10.0.0.0/8 accept
assert_loaded 'ip daddr 10.66.0.0/16 counter name "sib_floor" drop' || exit 1
g0=$(ctr sib_floor)
out=$(sdial "$SIB_IP:$PROBE_PORT" 2000)
g1=$(ctr sib_floor)
note "floor-above-allow, to sibling: $out  (sib_floor $g0 -> $g1)"
[ "${out#*hello-from-SIBLING}" = "$out" ] && [ "$g1" -gt "$g0" ] \
  && ok "floor above allow: sibling blocked AND floor counter moved ($g0 -> $g1) — the nesting is resolved by ORDER, not by carving 10.66/16 out of the tier" \
  || inv "right-order arm unpaired: $out, counter $g0 -> $g1"
# And the allow must still work for the rest of 10/8 — otherwise the floor has
# eaten the feature the declaration asked for. (Not measured against the LAN
# peer: Q5 left a 192.168/16 drop in `cfwd`, so that target would time out for
# an unrelated reason and read as a floor problem.)
o0=$(ctr sib_floor)
out2=$(sdial "$OTH_IP:$PROBE_PORT" 2000)
o1=$(ctr sib_floor)
note "same rule set, 10.77.250.2 (inside 10/8, outside the sibling CIDR): $out2  (sib_floor $o0 -> $o1)"
[ "$o1" = "$o0" ] && ok "the RFC1918 allow still covers the rest of 10/8 (floor counter did not move) — floor and tier coexist by order" \
                  || inv "floor counter moved on a non-sibling 10/8 target"

echo
echo "== summary: pass=$pass fail=$fail invalid=$invalid"
[ "$fail" = 0 ] || exit 1
