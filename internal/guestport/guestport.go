// Package guestport holds the facts both ends of the the documented invariant channel must agree on.
//
// It is a leaf with no imports, and that is the point. The port is the documented invariant's own fact, but
// the documented invariant needs it too — the documented invariant assigns endpoint discovery to the documented invariant, whose local_runsc composes
// IP:port into the placement handle. Reading it from internal/transport would make the documented invariant
// import the documented invariant's whole Connect-RPC + protobuf tree for one integer, on an edge pointing
// opposite to the spec's ownership. Here, both sides depend on a leaf and neither on the
// other. A shared substrate constant, not a seam: nothing is registered or swappable.
package guestport

// Port is the TCP port sandboxd listens on inside the guest netns. Constant across
// sandboxes because each has its own netns — the veth IP disambiguates, not the port.
const Port = 9000

// UploadTempPrefix prefixes the in-progress temp file of a streaming upload (the documented invariant fs /
// transport the documented invariant). The guest creates these; the documented invariant must EXCLUDE them from a Promoted layer,
// since a crash mid-upload strands one beside the caller's target and freezing it would
// ship the debris to every sandbox created from that Template. The temp file cannot just
// live in /tmp — rename(2) cannot cross filesystems — hence a coordinated prefix.
const UploadTempPrefix = ".sandboxd-upload-"
