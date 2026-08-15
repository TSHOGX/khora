#!/usr/bin/env bash
# spike-publish-ingress.sh — the network declaration ingress (`publish`): measure the INBOUND path,
# which the existing egress measurements do not cover.
#
# It exists to falsify one spec claim and answer two unmeasured shape questions:
#
#  Q1 (spec-changing) — the network declaration promised, at the time this
#     ran, that a guest service bound to 127.0.0.1 yields "the connection is
#     refused and the error is attributable, rather than hanging".
#     "Attributable" requires that this case be DISTINGUISHABLE from "nothing is
#     listening at all". If gVisor's netstack answers both with an RST, the
#     caller sees `refused` either way and the promise is unfalsifiable from
#     outside — the contract then has to be reworded to promise only "refused".
#     Case B vs case C is that comparison. It is: the measurement below confirmed
#     the two are indistinguishable, and the network declaration now stops at "refused and fails
#     fast", stating plainly that it does not promise attributability.
#
#  Q2 — which chain must carry the companion accept. Existing measurements cover the hook
#     split for EGRESS (guest→host lands in `input`, guest→routed lands in
#     `forward`). Publish is a different path: it arrives at `prerouting`, is
#     DNAT'd, and then lands somewhere the egress measurement does not predict.
#     Cases A/A-host measure where, by reading per-hook counters.
#
#  Q3 — whether the reverse flow (guest→caller reply) is carried by the shared
#     `established,related` arm, or needs a rule of its own. The network policy requires that
#     publish's reverse flow must NOT become an egress path, so the generator
#     needs the measured shape before it encodes one. Cases D/E are the pair:
#     same floor, with and without the established arm.
#
# Fixture: a real runsc guest (--network=sandbox, netstack) + a SECOND netns
# standing in for an external caller, so inbound traffic actually traverses
# prerouting/forward. Dialing only from the host would miss that path entirely:
# host-originated traffic goes output/postrouting and never sees a prerouting
# DNAT rule — itself a fact worth recording, so both are measured.
#
# Usage: sudo ./spike-publish-ingress.sh   (needs root: runsc, netns, nft)
set -euo pipefail

WORK="$(mktemp -d /tmp/spike-pub.XXXXXX)"
ROOTFS="$WORK/rootfs"; BUNDLE="$WORK/bundle"; RUNSC_ROOT="$WORK/runsc-root"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
CID="spikepub$$"
TABLE="spikepub$$"

# Sandbox side: the /30 shape local_runsc uses today within 10.66.0.0/16.
GNS="spikepub-g$$"; GVETH_H="spgh$$"; GVETH_G="spgg$$"
GHOST_IP="10.66.9.1"; GUEST_IP="10.66.9.2"
# Caller side: a distinct /30 outside the sandbox CIDR, so it stands in for
# "something external" rather than for a sibling sandbox.
CNS="spikepub-c$$"; CVETH_H="spch$$"; CVETH_C="spcc$$"
CHOST_IP="10.77.9.1"; CALLER_IP="10.77.9.2"

GUEST_PORT=8080      # what the guest service binds
PUB_PORT=18080       # the host-side published port
BANNER="hello-from-guest"
LOG="$WORK/guest.log"

pass=0; fail=0
ok()   { echo "  PASS: $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $*"; fail=$((fail+1)); }
note() { echo "  note: $*"; }

cleanup() {
  runsc --root="$RUNSC_ROOT" delete --force "$CID" 2>/dev/null || true
  nft delete table inet "$TABLE" 2>/dev/null || true
  ip netns del "$GNS" 2>/dev/null || true
  ip netns del "$CNS" 2>/dev/null || true
  ip link del "$GVETH_H" 2>/dev/null || true
  ip link del "$CVETH_H" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== build pubprobe (static) + minimal rootfs =="
CGO_ENABLED=0 go build -o "$WORK/pubprobe" "$REPO/test/preflight/pubprobe"
"$REPO/test/preflight/build-rootfs.sh" "$ROOTFS" "$WORK/pubprobe" >/dev/null
# build-rootfs installs the binary at /sbin/sandboxd; that is just the entrypoint
# path it hardcodes. Give it its own name too, so the OCI args read honestly.
cp "$WORK/pubprobe" "$ROOTFS/usr/bin/pubprobe"

echo "== caller netns (stands in for an external client) =="
ip netns add "$CNS"
ip link add "$CVETH_H" type veth peer name "$CVETH_C"
ip link set "$CVETH_C" netns "$CNS"
ip addr add "$CHOST_IP/30" dev "$CVETH_H"; ip link set "$CVETH_H" up
ip netns exec "$CNS" ip addr add "$CALLER_IP/30" dev "$CVETH_C"
ip netns exec "$CNS" ip link set "$CVETH_C" up
ip netns exec "$CNS" ip link set lo up
ip netns exec "$CNS" ip route add default via "$CHOST_IP" dev "$CVETH_C"

# launch_guest <mode> — (re)create the sandbox netns and run the guest.
# mode: "any" = bind 0.0.0.0, "loopback" = bind 127.0.0.1, "none" = no listener.
launch_guest() {
  local mode="$1" args
  case "$mode" in
    any)      args='["/usr/bin/pubprobe","serve","0.0.0.0:'"$GUEST_PORT"'","'"$BANNER"'"]' ;;
    loopback) args='["/usr/bin/pubprobe","serve","127.0.0.1:'"$GUEST_PORT"'","'"$BANNER"'"]' ;;
    none)     args='["/usr/bin/pubprobe","idle"]' ;;
    *) echo "bad mode $mode" >&2; return 1 ;;
  esac

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

  mkdir -p "$BUNDLE"
  cat > "$BUNDLE/config.json" <<EOF
{
  "ociVersion": "1.0.0",
  "process": { "terminal": false, "user": {"uid":0,"gid":0},
    "args": $args, "cwd": "/",
    "env": ["PATH=/usr/bin:/bin:/usr/sbin:/sbin"] },
  "root": { "path": "$ROOTFS", "readonly": false },
  "hostname": "spikepub",
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
  # deadlock); redirect to a file, which is also how we observe the listener.
  runsc --root="$RUNSC_ROOT" --platform=systrap --network=sandbox \
    --overlay2=none --file-access=shared --systemd-cgroup=false \
    run --detach --bundle="$BUNDLE" "$CID" >"$LOG" 2>&1

  # Wait for the guest to report its state rather than sleeping blind.
  local want deadline
  case "$mode" in
    none) want="IDLE_NO_LISTENER" ;;
    *)    want="SERVE_LISTENING\|SERVE_BIND_FAILED" ;;
  esac
  deadline=$((SECONDS+15))
  while (( SECONDS < deadline )); do
    grep -q "$want" "$LOG" && break
    sleep 0.2
  done
  if ! grep -q "$want" "$LOG"; then
    echo "  !! guest did not reach expected state ($want); log:" >&2
    sed 's/^/     /' "$LOG" >&2
    return 1
  fi
  note "guest[$mode]: $(grep -o "$want[^ ]*.*" "$LOG" | head -1)"
}

# install_pub <floor> — the publish rule set. floor:
#   none        DNAT + forward accept only (isolates the ingress path)
#   ct          floor WITH the shared `established,related` arm (Q3 positive)
#   noct        same floor WITHOUT that arm (Q3 negative — must break the reply)
# Counters are named so each hook can be read independently (measurement discipline:
# "it works" is not an assertion; a counter that stays 0 is the real signal).
install_pub() {
  local floor="$1"
  nft delete table inet "$TABLE" 2>/dev/null || true
  nft -f - <<EOF
table inet $TABLE {
  counter pre_dnat {}
  counter out_dnat {}
  counter in_est {}
  counter in_new_drop {}
  counter fw_all {}
  counter fw_est {}
  counter fw_pub {}
  counter fw_floor_drop {}
  counter post_snat {}
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "$CVETH_H" tcp dport $PUB_PORT counter name pre_dnat \
      dnat ip to $GUEST_IP:$GUEST_PORT
  }
  chain output_nat {
    type nat hook output priority dstnat; policy accept;
    ip daddr $CHOST_IP tcp dport $PUB_PORT counter name out_dnat \
      dnat ip to $GUEST_IP:$GUEST_PORT
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    # MEASURED, and load-bearing: without this SNAT, publish only works if the
    # guest has a default route (a reply to an off-/30 caller is otherwise
    # unroutable from inside the netstack). SNAT rewrites the source to the
    # sandbox's own /30 gateway, so the reply is on-link and needs no guest
    # route at all — which is what makes publish a PURELY host-side rule, as
    # the network declaration claims. Verified both ways: DNAT-only + no guest route = timeout;
    # DNAT + this rule + no guest route = connected.
    oifname "$GVETH_H" ip daddr $GUEST_IP counter name post_snat snat ip to $GHOST_IP
  }
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "$GVETH_H" ct state established,related counter name in_est accept
    iifname "$GVETH_H" counter name in_new_drop drop
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
$(case "$floor" in
  none) echo "    counter name fw_all accept" ;;
  ct)   cat <<'ARM'
    ct state established,related counter name fw_est accept
    tcp dport GUESTPORT_PLACEHOLDER counter name fw_pub accept
    iifname "GVETH_PLACEHOLDER" counter name fw_floor_drop drop
ARM
        ;;
  noct) cat <<'ARM'
    tcp dport GUESTPORT_PLACEHOLDER counter name fw_pub accept
    iifname "GVETH_PLACEHOLDER" counter name fw_floor_drop drop
ARM
        ;;
esac | sed -e "s/GUESTPORT_PLACEHOLDER/$GUEST_PORT/" -e "s/GVETH_PLACEHOLDER/$GVETH_H/")
  }
}
EOF
}

ctr() { nft -j list counters table inet "$TABLE" 2>/dev/null \
        | python3 -c 'import sys,json
d=json.load(sys.stdin)
want=sys.argv[1]
for o in d["nftables"]:
    c=o.get("counter")
    if c and c.get("name")==want: print(c["packets"]); break
else: print("NA")' "$1"; }

dial_from_caller() { ip netns exec "$CNS" "$WORK/pubprobe" dial "$CHOST_IP:$PUB_PORT" 3000; }
dial_from_host()   { "$WORK/pubprobe" dial "$CHOST_IP:$PUB_PORT" 3000; }
dial_guest_direct() { ip netns exec "$CNS" "$WORK/pubprobe" dial "$GUEST_IP:$GUEST_PORT" 3000; }
field() { sed -n "s/.*$1=\([^ ]*\).*/\1/p" <<<"$2"; }

echo
echo "===== CASE A: guest binds 0.0.0.0 — publish must work (Q2: which hook) ====="
launch_guest any
install_pub none
r="$(dial_from_caller)"; echo "  caller: $r"
[[ "$(field OUTCOME "$r")" == connected ]] \
  && ok "A/caller reachable, payload=$(field PAYLOAD "$r")" \
  || bad "A/caller expected connected, got $(field OUTCOME "$r")"
note "pre_dnat=$(ctr pre_dnat) fw_all=$(ctr fw_all) in_est=$(ctr in_est) in_new_drop=$(ctr in_new_drop)"
if [[ "$(ctr fw_all)" != "0" && "$(ctr fw_all)" != "NA" ]]; then
  note "Q2: DNAT'd publish traffic traverses FORWARD (fw_all=$(ctr fw_all))"
else
  note "Q2: publish traffic did NOT increment forward; it landed elsewhere"
fi

echo
echo "-- A-host: same rule set, dialed from the HOST (different hook path) --"
r="$(dial_from_host)"; echo "  host:   $r"
note "out_dnat=$(ctr out_dnat) (prerouting is not traversed by host-originated traffic)"
[[ "$(field OUTCOME "$r")" == connected ]] \
  && ok "A/host reachable (needed its own output-hook DNAT rule)" \
  || bad "A/host got $(field OUTCOME "$r") — host path needs a separate rule"

echo
echo "===== CASE B: guest binds 127.0.0.1 — the contract's claim (Q1) ====="
launch_guest loopback
install_pub none
rB="$(dial_from_caller)"; echo "  caller: $rB"
outB="$(field OUTCOME "$rB")"; msB="$(field ELAPSED_MS "$rB")"
[[ "$outB" != timeout ]] \
  && ok "B not hung (outcome=$outB, ${msB}ms) — the 'does not hang' half holds" \
  || bad "B HUNG (${msB}ms) — contract says it must be refused, not hang"

echo
echo "===== CASE C: nothing listening — the control for B (Q1) ====="
launch_guest none
install_pub none
rC="$(dial_from_caller)"; echo "  caller: $rC"
outC="$(field OUTCOME "$rC")"; msC="$(field ELAPSED_MS "$rC")"
[[ "$outC" != timeout ]] && note "C also fails fast (outcome=$outC, ${msC}ms)" \
                         || note "C hangs (outcome=$outC, ${msC}ms)"

echo
echo "-- Q1 VERDICT: is B distinguishable from C? --"
if [[ "$outB" == "$outC" ]]; then
  bad "Q1: B and C are INDISTINGUISHABLE (both $outB) — 'the error is attributable'"
  bad "    is NOT observable by the caller. The network declaration must be reworded to"
  bad "    promise only 'refused', dropping the attributability claim."
else
  ok "Q1: distinguishable (B=$outB, C=$outC) — 'attributable' holds as written"
fi

echo
echo "===== CASE D: floor WITH the established arm — reverse flow (Q3) ====="
launch_guest any
install_pub ct
r="$(dial_from_caller)"; echo "  caller: $r"
[[ "$(field OUTCOME "$r")" == connected ]] \
  && ok "D publish works under a real floor (fw_est=$(ctr fw_est) fw_pub=$(ctr fw_pub))" \
  || bad "D expected connected under floor, got $(field OUTCOME "$r")"
note "fw_est=$(ctr fw_est) fw_pub=$(ctr fw_pub) fw_floor_drop=$(ctr fw_floor_drop)"

echo
echo "-- D-egress: the floor must still block guest-initiated egress --"
r="$(ip netns exec "$GNS" "$WORK/pubprobe" dial "$CALLER_IP:9999" 2000 || true)"
echo "  guest→caller: $r"
[[ "$(field OUTCOME "$r")" != connected ]] \
  && ok "D floor still denies guest egress (drop=$(ctr fw_floor_drop))" \
  || bad "D floor did NOT deny guest egress — publish's arm became an egress path"

echo
echo "===== CASE E: same floor WITHOUT the established arm (Q3 negative) ====="
launch_guest any
install_pub noct
r="$(dial_from_caller)"; echo "  caller: $r"
outE="$(field OUTCOME "$r")"
if [[ "$outE" == connected ]]; then
  note "Q3: publish works WITHOUT the established arm — the reply is carried by"
  note "    the dport accept alone (fw_pub=$(ctr fw_pub)), so that arm is not"
  note "    load-bearing for ingress."
else
  ok "Q3: removing the established arm BREAKS publish (outcome=$outE,"
  ok "    fw_floor_drop=$(ctr fw_floor_drop)) — the arm is load-bearing and the"
  ok "    generator must emit it; publish needs no separate reverse-flow rule."
fi

echo
echo "===== SUMMARY ====="
echo "  pass=$pass fail=$fail"
echo
echo "  Q1 (loopback-bind attributability): B=$outB vs C=$outC"
echo "  Q2 (which hook carries DNAT'd publish): see CASE A counters above"
echo "  Q3 (reverse flow / established arm): see CASE D vs E above"
[[ "$fail" -eq 0 ]] || echo "  (a FAIL above is a measurement result, not a script error — read it)"
