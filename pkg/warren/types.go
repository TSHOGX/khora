package warren

// The §6.1 wire types.
//
// EVERY FIELD HERE IS ONE §6.1 FIELD, in the same shape, with the same json tag —
// ADR-0008's first rule for what "thin" means. That is a duplication of
// internal/orchestrator's own request/response structs, and it is deliberate rather
// than an oversight: those types are unexported inside `internal/`, so a consumer
// cannot name them at all, and exporting them would make the engine's internal
// handler shapes part of a published API that then could not be refactored.
//
// The duplication is kept honest MECHANICALLY, not by discipline: contract_test.go
// diffs these types against docs/spec/openapi.json — the document that is itself
// generated from the handler types — so a field that drifts on either side fails a
// test. That is the same trick the document uses on the handlers, applied one layer
// out.
//
// Two conventions inherited from the handlers, both load-bearing:
//   - A POINTER means "omitted is distinguishable from zero". On a PATCH that is the
//     difference between "leave this axis alone" and "set it to 0", and 0 is itself
//     meaningful (`0 = unlimited` for a cap, `retention: 0` = never evict).
//   - A duration crosses the wire as a Go duration STRING ("30m", "24h"), so the unit
//     is explicit. Use Duration() to build one.

// SandboxState is one of §4.1's five spellings. A string, not an enum: the engine is
// authoritative for the set, and a Go constant that fell behind it would be worse than
// no constant. Compare against the exported values below.
type SandboxState = string

// The §4.1 states. Present as constants because a caller branching on state is the
// common case and a typo'd literal is silent.
const (
	StateCreating SandboxState = "creating"
	StateRunning  SandboxState = "running"
	StatePausing  SandboxState = "pausing"
	StatePaused   SandboxState = "paused"
	StateDead     SandboxState = "dead"
)

// EventKind is one of §4.1's SIX kinds. The set is CLOSED (§4.1, and a SQLite CHECK
// enforces it), so these six are exhaustive — a seventh spelling means the engine was
// changed, not that this list is short.
type EventKind = string

const (
	KindCreated    EventKind = "created"
	KindStarted    EventKind = "started"
	KindAutoPaused EventKind = "auto-paused"
	KindResumed    EventKind = "resumed"
	KindDied       EventKind = "died"
	KindDestroyed  EventKind = "destroyed"
)

// CreateRequest is POST /v1/sandboxes.
type CreateRequest struct {
	TemplateID string `json:"template_id"`
	// IdleTimeout / Retention are duration strings; "" means the engine-wide default.
	IdleTimeout string `json:"idle_timeout,omitempty"`
	Retention   string `json:"retention,omitempty"`
	// Resource limits. 0 = unlimited on that axis. Three axes, never one blended
	// "size" — they are not interchangeable.
	CPUMillis  int64 `json:"cpu_millis,omitempty"`
	MemoryByte int64 `json:"memory_byte,omitempty"`
	DiskByte   int64 `json:"disk_byte,omitempty"`
	// Volumes is the COMPLETE set of mounts this sandbox is created with. `access` is
	// given here and not on the volume record, because it is a property of the MOUNT.
	Volumes []VolumeBinding `json:"volumes,omitempty"`
	// Network is optional as a whole: omitting it normalizes to deny-all egress with no
	// exceptions. But GIVING the object and leaving out Egress.Default is a 400 — hence
	// a pointer, so the two cases stay distinguishable here as they are on the wire.
	Network *Network `json:"network,omitempty"`
}

// VolumeBinding is one entry of a `volumes` binding, on create and on Patch. One type
// for both, because a PATCH swaps a whole binding for a whole binding.
type VolumeBinding struct {
	VolumeID string `json:"volume_id"`
	Path     string `json:"path"`
	Access   string `json:"access"` // "ro" | "rw"
}

// Sandbox is what GET /v1/sandboxes/{id} and each entry of the list answer with. One
// type for both — the engine deliberately returns the identical shape, so a caller
// that finds several `paused` in a list need not fan out one GET per ID to learn which
// are resumable.
type Sandbox struct {
	SandboxID string       `json:"sandbox_id"`
	State     SandboxState `json:"state"`
	// DeathCause is why the isolation unit was last found dead. Absent = never observed
	// dead, which is why it is "" rather than a "healthy" spelling.
	DeathCause string `json:"death_cause,omitempty"`
	// UsedByte is what this sandbox's overlay consumes (upper + work). NIL means
	// unreadable, never 0 — a pointer because a fresh sandbox's usage genuinely IS 0,
	// so the collision between "empty" and "unreadable" is the common case here.
	UsedByte *int64 `json:"used_byte,omitempty"`
}

// PatchRequest rewrites a sandbox's binding. Every field is a POINTER so "omitted" is
// distinguishable from "set to zero" — see the file header for why that matters on
// these particular axes.
type PatchRequest struct {
	CPUMillis   *int64  `json:"cpu_millis,omitempty"`
	MemoryByte  *int64  `json:"memory_byte,omitempty"`
	DiskByte    *int64  `json:"disk_byte,omitempty"`
	IdleTimeout *string `json:"idle_timeout,omitempty"`
	Retention   *string `json:"retention,omitempty"`
	// Volumes is a pointer to a SLICE for the same reason: nil leaves the binding
	// alone, while a non-nil empty slice removes every mount.
	Volumes *[]VolumeBinding `json:"volumes,omitempty"`
	// Network swaps the §5.D declaration WHOLE for WHOLE — there is no field-level edit
	// of a declaration, because merge semantics on an allow-list is how a rule gets
	// dropped without anyone deciding to. Prefer SetNetwork, which is this field with
	// the required-field check attached.
	Network *Network `json:"network,omitempty"`
}

// VerbResult is what Pause and Resume answer with. State is read back AFTER the verb,
// so a caller sees the achieved state rather than the one it asked for.
type VerbResult struct {
	SandboxID string       `json:"sandbox_id"`
	State     SandboxState `json:"state"`
}

// ActivityResult is what RecordActivity answers with: the idle duration measured IN
// THE GUEST after the stamp landed. A reading, not an acknowledgement — a caller gets
// positive confirmation that the clock moved.
type ActivityResult struct {
	IdleMillis int64 `json:"idle_millis"`
}

// Template is one entry of GET /v1/templates.
type Template struct {
	TemplateID string `json:"template_id"`
	// Parent is the sandbox this Template was promoted from, "" for a builtin.
	Parent  string `json:"parent"`
	Builtin bool   `json:"builtin"`
}

// Event is one §4.4-C outbox entry.
//
// Delivery is AT-LEAST-ONCE, so a caller MUST dedup by ID — Seq is the cursor, ID is
// the identity, and they are different fields for that reason.
type Event struct {
	Seq       int64        `json:"seq"`
	ID        string       `json:"id"`
	SandboxID string       `json:"sandbox_id"`
	Kind      EventKind    `json:"kind"`
	State     SandboxState `json:"state"`
	// Cause is present on `died` and on a `destroyed` that followed a death — the
	// reason the isolation unit was found dead. Absent otherwise.
	Cause      string `json:"cause,omitempty"`
	OccurredAt string `json:"occurred_at"`
}

// EventPage is one answer from Events.
type EventPage struct {
	Events []Event `json:"events"`
	// Earliest / Latest bound the retained window (0/0 when the outbox is empty). They
	// are what makes a 410 actionable: a caller whose cursor fell below the window
	// restarts from Earliest rather than guessing.
	Earliest int64 `json:"earliest"`
	Latest   int64 `json:"latest"`
}

// Volume is what the volume reads answer with.
type Volume struct {
	VolumeID string `json:"volume_id"`
	Size     int64  `json:"size"`
	Backend  string `json:"backend,omitempty"`
	// UsedByte is nil when unreadable, never 0 — same rule as Sandbox.UsedByte, for the
	// same reason.
	UsedByte *int64 `json:"used_byte,omitempty"`
}

// VolumeCreateRequest is POST /v1/volumes.
type VolumeCreateRequest struct {
	Size int64 `json:"size"`
}

// VolumePatchRequest resizes a volume. A pointer for the omitted-vs-zero reason.
type VolumePatchRequest struct {
	Size *int64 `json:"size,omitempty"`
}

// VolumeWriteResult is what an import answers with.
type VolumeWriteResult struct {
	VolumeID     string `json:"volume_id"`
	Path         string `json:"path"`
	BytesWritten int64  `json:"bytes_written"`
}
