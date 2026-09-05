// Package guestport holds the small facts shared by the runtime provider and
// the guest startup topology. Keeping these values in an inert leaf avoids a
// runtime↔transport dependency for configuration that both sides must parse.
//
// It is a leaf with no imports, and that is the point. The port is the transport data plane's own fact, but
// the runtime provider contract needs it too — the transport data plane assigns endpoint discovery to the runtime provider contract, whose local_runsc composes
// IP:port into the placement handle. Reading it from internal/transport would make the runtime provider contract
// import the transport data plane's whole Connect-RPC + protobuf tree for one integer, reversing
// the mechanism ownership boundary. Here, both sides depend on a leaf and neither on the
// other. A shared substrate constant, not a seam: nothing is registered or swappable.
package guestport

// Port is the TCP port sandboxd listens on inside the guest netns. Constant across
// sandboxes because each has its own netns — the veth IP disambiguates, not the port.
const Port = 9000

// ProcessRetentionEnv is the private launch-time configuration key passed to
// sandboxd PID 1 by the runtime provider.  It carries the same portable
// process-retention policy that the control plane applies to its durable
// Process rows; the value is encoded as signed nanoseconds so the guest can
// parse it without importing an engine package.
const ProcessRetentionEnv = "KHORA_PROCESS_RETENTION_NS"

// UploadTempPrefix prefixes the in-progress temp file of a streaming upload (the transport data plane fs /
// fs transport). The guest creates these; the Rootfs subsystem must EXCLUDE them from a Promoted layer,
// since a crash mid-upload strands one beside the caller's target and freezing it would
// ship the debris to every sandbox created from that Template. The temp file cannot just
// live in /tmp — rename(2) cannot cross filesystems — hence a coordinated prefix.
const UploadTempPrefix = ".sandboxd-upload-"
