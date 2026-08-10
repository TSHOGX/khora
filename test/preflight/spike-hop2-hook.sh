#!/usr/bin/env bash
# spike-hop2-hook.sh — WHERE is the second hop (§2.4 invariant 2) evaluable?
#
# §2.4's second invariant says both hops are pinned and the second one is
# enforced BY THE KERNEL, not by the via-process's own discipline. D4.2 works out
# the predicate (uid/gid, not mark) and the two relative positions (a RETURN above
# the steering rules so the process does not re-match its own rule; the L0 floor
# ABOVE that, so it stays subject to the floor). But D4.2 also asserts the whole
# predicate "只活在 forward" — and that claim is unmeasured.
#
# It looks mechanically wrong: hop 2 is a HOST-LOCAL process opening a NEW
# connection (§5.D requires via[].endpoint to be host-local, §6.2 400s anything
# else). Locally-originated packets are routed out via `output`/`postrouting`;
# they never traverse `forward`, which only sees packets being routed BETWEEN
# interfaces. And `skuid` needs a socket owner — in `forward` there is no local
# socket to ask.
#
# If that reading is right, three things follow and all three are load-bearing
# for the rule-set shape:
#   * the L0 floor as designed (input + forward, iifname-narrowed) sees NONE of
#     hop 2 — the confused-deputy path is wide open, and no floor counter moves,
#     which is the silent-pass signature D4.1 warns about;
#   * the hop-2 rules need an `output` base chain, which is NOT per-sandbox
#     (one uid, one deployment) — so it cannot live in a per-sandbox table, and
#     the render guard "every rule carries iifname" cannot apply to it;
#   * that `output` chain needs its own narrowing guard, for exactly guard ②'s
#     reason: an un-narrowed drop there is a HOST-level egress kill switch.
#
# So this script measures, for a host-local process's egress:
#   Q1  which hooks see it: forward / output / postrouting
#   Q2  whether `meta skuid` matches, per hook
#   Q3  whether an iifname-narrowed floor (the L0 shape) sees it at all
#   Q4  whether a skuid-narrowed drop in `output` actually enforces
#   Q5  whether an un-narrowed drop in `output` is a host-wide kill switch
#
# Target of the probe = a peer netns on a /30, standing in for the sibling
# address space the floor must cover (D5 ④). Root required.
#
# ── MEASURED (nft 1.1.6, this machine) ────────────────────────────────────────
#
# Q1/Q3  A host-local process's egress is INVISIBLE to `forward`, in all three
#        shapes: fwd_skuid=0, fwd_any=0, fwd_iifname=0 — while out_any=5 saw the
#        whole flow. So the L0 floor as designed (input + forward, iifname-narrowed)
#        sees NONE of hop 2, and no floor counter moves: the silent-pass signature.
#        `postrouting` also counted 0 — the nat hook only evaluates the first packet
#        of a flow and this table installed no NAT, so it is not an enforcement
#        point either.
# Q2     `meta skuid` DOES match in `output` (out_skuid=4..5 of the flow).
# Q2b    …but NOT reliably every packet. Direct negative test read noown=0/5 (all
#        owned), while Q1 read out_skuid=4 vs out_any=5 in 2 of 3 runs — i.e. one
#        packet occasionally carries no socket owner (a FIN emitted after the
#        socket is orphaned at process exit is the plausible one; not pinned).
#        ⟹ ASYMMETRIC CONSEQUENCE, and this is the one to design around: a
#        skuid-narrowed DROP is safe (dropping the SYN kills the flow), but a
#        skuid-narrowed RETURN/accept above a floor drop is NOT reliable — the
#        packets skuid fails to match fall through to whatever is below.
# Q4     A skuid-narrowed drop in `output` enforces: probe as that uid FAILS with
#        counter=3, while the same probe as root gets the banner. Narrowing holds.
# Q5     An UN-narrowed drop in `output` is a HOST-WIDE egress kill switch: root's
#        probe fails too (counter=6). This is guard ②'s hazard on a second hook —
#        an output-hook rule missing its skuid narrowing takes out the host, and
#        `iifname` cannot be that narrowing here (nothing arrives on an interface).
# Q5b    `policy drop` on an output base chain IS effectively narrowable by a
#        `skuid !=` accept above it (root got the banner) — unlike `iifname` on a
#        per-sandbox chain, which guard ② measured as un-narrowable. Do not read
#        this as permission to use `policy drop`: it works only because skuid is
#        evaluable for every locally-owned packet, and Q2b shows that is not
#        universal.
#
# ⟹ D4.2's "this predicate lives entirely in `forward`" is WRONG as written; the
#    hook is `output`. Consequences for the rule-set shape are written up in
#    network.md D5 ㉔.
set -u

HOP2_UID=12345
PEER_NS=spikehop2
VETH_H=vh-hop2
VETH_P=vp-hop2
HOST_IP=10.77.0.1
PEER_IP=10.77.0.2
PORT=9377
TABLE=spike_hop2

say() { printf '\n=== %s ===\n' "$*"; }
cnt() { nft -j list counters table inet $TABLE 2>/dev/null |
        python3 -c "import json,sys
d=json.load(sys.stdin)
for o in d['nftables']:
  c=o.get('counter')
  if c: print(f\"{c['name']}={c['packets']}\")" ; }

cleanup() {
  nft delete table inet $TABLE 2>/dev/null
  ip netns pids $PEER_NS 2>/dev/null | xargs -r kill 2>/dev/null
  ip netns del $PEER_NS 2>/dev/null
  ip link del $VETH_H 2>/dev/null
}
trap cleanup EXIT
cleanup

say "setup: peer netns as the probe target ($PEER_IP:$PORT)"
ip netns add $PEER_NS
ip link add $VETH_H type veth peer name $VETH_P
ip link set $VETH_P netns $PEER_NS
ip addr add $HOST_IP/30 dev $VETH_H
ip link set $VETH_H up
ip -n $PEER_NS addr add $PEER_IP/30 dev $VETH_P
ip -n $PEER_NS link set $VETH_P up
ip -n $PEER_NS link set lo up
ip netns exec $PEER_NS python3 -c "
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(('0.0.0.0',$PORT)); s.listen(8)
while True:
    c,_=s.accept(); c.sendall(b'hello-from-PEER\n'); c.close()
" &
sleep 1

# probe = a connect as HOP2_UID, standing in for the via[] process. Prints the
# banner on success, an errno-ish word on failure, so Q4/Q5 read the SAME way.
probe() {
  setpriv --reuid=$HOP2_UID --regid=$HOP2_UID --clear-groups \
    timeout 4 python3 -c "
import socket,sys
try:
    s=socket.create_connection(('$PEER_IP',$PORT),timeout=3)
    sys.stdout.write(s.recv(64).decode().strip())
except Exception as e:
    sys.stdout.write('FAIL:'+type(e).__name__)
" 2>/dev/null || printf 'FAIL:timeout'
  echo
}
# same probe as root — Q5's control arm (is the drop host-wide, or only that uid?)
probe_root() {
  timeout 4 python3 -c "
import socket,sys
try:
    s=socket.create_connection(('$PEER_IP',$PORT),timeout=3)
    sys.stdout.write(s.recv(64).decode().strip())
except Exception as e:
    sys.stdout.write('FAIL:'+type(e).__name__)
" 2>/dev/null || printf 'FAIL:timeout'
  echo
}

say "Q1+Q2+Q3: which hooks see a host-local process's egress, and does skuid match"
# Observation-only: every rule is a bare counter, no verdict. Three hooks x
# {skuid-narrowed, unnarrowed}, plus the L0-shaped iifname floor for Q3.
# NOTE two rendering constraints, both learned the hard way here:
#  * named counters must be DECLARED BEFORE the chains referencing them in one
#    -f file;
#  * `fwd` is a RESERVED WORD in nft 1.1.6 and cannot be a chain name. It fails
#    with "unexpected fwd, expecting string or last" — and because the whole file
#    is one parse, that desynchronizes the parser and every LATER chain reports a
#    bogus error of its own. So a chain-name collision presents as "my rule syntax
#    is wrong", several chains away from the actual cause.
nft -f - <<EOF
table inet $TABLE {
  counter fwd_skuid {}
  counter fwd_any {}
  counter fwd_iifname {}
  counter out_skuid {}
  counter out_any {}
  counter post_skuid {}
  counter post_any {}
  chain hop2fwd { type filter hook forward priority 0; policy accept;
    meta skuid $HOP2_UID counter name fwd_skuid
    ip daddr $PEER_IP counter name fwd_any
    iifname "$VETH_H" counter name fwd_iifname
  }
  chain out { type filter hook output priority 0; policy accept;
    meta skuid $HOP2_UID counter name out_skuid
    ip daddr $PEER_IP counter name out_any
  }
  chain post { type nat hook postrouting priority 100; policy accept;
    meta skuid $HOP2_UID counter name post_skuid
    ip daddr $PEER_IP counter name post_any
  }
}
EOF
echo "probe as uid $HOP2_UID -> $(probe)"
cnt

say "Q2b: does skuid match EVERY packet of the flow, or only the socket-owned ones?"
# Q1 measured out_skuid=4 vs out_any=5 — one packet of the flow did NOT carry an
# owner. Which one decides the SHAPE hop-2 rules may take: a skuid-narrowed DROP
# only needs to catch the SYN, but a skuid-narrowed RETURN/accept sitting above a
# floor drop leaks every packet skuid fails to match.
nft delete table inet $TABLE
nft -f - <<EOF
table inet $TABLE {
  counter allp {}
  counter noown {}
  counter noown_syn {}
  counter noown_fin {}
  counter noown_rst {}
  counter noown_ack {}
  chain out { type filter hook output priority 0; policy accept;
    ip daddr $PEER_IP counter name allp
    ip daddr $PEER_IP meta skuid != $HOP2_UID counter name noown
    ip daddr $PEER_IP meta skuid != $HOP2_UID tcp flags syn counter name noown_syn
    ip daddr $PEER_IP meta skuid != $HOP2_UID tcp flags fin counter name noown_fin
    ip daddr $PEER_IP meta skuid != $HOP2_UID tcp flags rst counter name noown_rst
    ip daddr $PEER_IP meta skuid != $HOP2_UID tcp flags ack counter name noown_ack
  }
}
EOF
echo "probe as uid $HOP2_UID -> $(probe)"
cnt

say "Q4: does a skuid-narrowed drop in \`output\` actually enforce?"
nft delete table inet $TABLE
nft -f - <<EOF
table inet $TABLE {
  chain out { type filter hook output priority 0; policy accept;
    meta skuid $HOP2_UID ip daddr $PEER_IP counter name q4_drop drop
  }
  counter q4_drop {}
}
EOF
echo "probe as uid $HOP2_UID -> $(probe)   (want: FAIL + counter > 0)"
echo "probe as root        -> $(probe_root)   (want: banner — narrowing holds)"
cnt

say "Q5: is an UN-narrowed drop in \`output\` a host-wide egress kill switch?"
nft delete table inet $TABLE
nft -f - <<EOF
table inet $TABLE {
  chain out { type filter hook output priority 0; policy accept;
    ip daddr $PEER_IP counter name q5_drop drop
  }
  counter q5_drop {}
}
EOF
echo "probe as uid $HOP2_UID -> $(probe)"
echo "probe as root        -> $(probe_root)   (want: FAIL — proves it is host-wide)"
cnt

say "Q5b: control — is \`policy drop\` on an \`output\` base chain narrowable at all?"
# Guard ② measured that chain policy cannot be narrowed by iifname. Same question
# for the output hook: if policy drop there is host-wide regardless of any rule,
# the hop-2 chain needs the same "policy accept + narrowed last rule" shape.
nft delete table inet $TABLE
nft -f - <<EOF
table inet $TABLE {
  chain out { type filter hook output priority 0; policy drop;
    meta skuid != $HOP2_UID counter name q5b_pass accept
  }
  counter q5b_pass {}
}
EOF
echo "probe as root -> $(probe_root)   (if FAIL: policy drop is host-wide even with an accept above it)"
cnt
nft delete table inet $TABLE

say done
