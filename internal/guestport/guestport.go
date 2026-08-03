// Package guestport holds the one fact both ends of the §5.E channel must agree
// on: the TCP port sandboxd listens on inside the guest netns.
//
// It is a leaf with NO imports, and that is the point. The port is §5.E's own
// fact — sandboxd chose it — but §5.A also needs it, because §5.E assigns
// endpoint discovery to §5.A ("宿主如何拿到 IP 归 §5.A"): local_runsc allocates
// the veth IP and composes IP:port into the placement handle. Having §5.A read it
// from internal/transport made the §5.A subsystem import §5.E and inherit its
// whole Connect-RPC + protobuf dependency tree — 34 non-stdlib packages to read
// one integer, and an edge pointing the opposite way from the spec's ownership
// (§5.E depends on §5.A's result, not the reverse).
//
// So the constant lives here instead, where both sides depend on a leaf and
// neither depends on the other. This is a shared substrate constant, not a seam:
// nothing is registered, no behavior is abstracted, and there is nothing to swap.
package guestport

// Port is the fixed TCP port sandboxd (transport mode) listens on inside the
// guest netns. It can be constant across sandboxes because each sandbox has its
// own netns — the veth IP disambiguates, not the port (§5.E netns-internal TCP).
const Port = 9000
