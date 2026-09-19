package khora

// The control-plane API wire types.
//
// EVERY FIELD HERE IS ONE the control-plane API FIELD, in the same shape, with the same json tag —
// the thin-client contract's first rule for what "thin" means. That is a duplication of
// internal/orchestrator's own request/response structs, and it is deliberate rather
// than an oversight: those types are unexported inside `internal/`, so a consumer
// cannot name them at all, and exporting them would make the engine's internal
// handler shapes part of a published API that then could not be refactored.
//
// The duplication is kept honest MECHANICALLY, not by discipline: contract_test.go
// diffs these types against docs/api/openapi.json — the document that is itself
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

// SandboxState is one of the lifecycle state machine's four spellings. A string, not an enum: the engine is
// authoritative for the set, and a Go constant that fell behind it would be worse than
// no constant. Compare against the exported values below.
type SandboxState = string

// IdempotencyKey is supplied on create requests. The engine persists it with
// a semantic request hash so retries converge across orchestrator restarts.
type IdempotencyKey string

// The lifecycle state machine states. Present as constants because a caller branching on state is the
// common case and a typo'd literal is silent.
const (
	StateCreating  SandboxState = "creating"
	StateRunning   SandboxState = "running"
	StatePaused    SandboxState = "paused"
	StateDestroyed SandboxState = "destroyed"
)

// EventKind is one of the lifecycle state machine's SIX kinds. The set is CLOSED (the lifecycle state machine, and a SQLite CHECK
// enforces it), so these six are exhaustive — a seventh spelling means the engine was
// changed, not that this list is short.
type EventKind = string

const (
	KindCreated     EventKind = "created"
	KindPaused      EventKind = "paused"
	KindAutoPaused  EventKind = "auto-paused"
	KindCrashPaused EventKind = "crash-paused"
	KindResumed     EventKind = "resumed"
	KindDestroyed   EventKind = "destroyed"
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
	// MutationRevision is an opaque validator returned by Get for conditional
	// lifecycle writes. It is absent from List; inspect before mutating.
	MutationRevision string       `json:"mutation_revision,omitempty"`
	SandboxID        string       `json:"sandbox_id"`
	State            SandboxState `json:"state"`
	PlacementEpoch   uint64       `json:"placement_epoch,omitempty"`
	PlacementState   string       `json:"placement_state,omitempty"`
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
	// Network swaps the network declaration WHOLE for WHOLE — there is no field-level edit
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
	// Parent is the immediate parent Template in the immutable lineage, "" for a builtin.
	Parent   string           `json:"parent"`
	Builtin  bool             `json:"builtin"`
	Manifest TemplateManifest `json:"manifest"`
}

// TemplateManifest is the immutable declaration attached to a Template.
type TemplateManifest struct {
	SchemaVersion   uint32   `json:"schema_version"`
	DigestAlgorithm string   `json:"digest_algorithm,omitempty"`
	ContentDigest   string   `json:"content_digest,omitempty"`
	Platform        string   `json:"platform"`
	Capabilities    []string `json:"capabilities"`
	Provenance      string   `json:"provenance,omitempty"`
	SBOMReference   string   `json:"sbom_reference,omitempty"`
}

type PromoteRequest struct {
	FromSandboxID string            `json:"from_sandbox_id"`
	Manifest      *TemplateManifest `json:"manifest,omitempty"`
}

// Event is one the transactional outbox entry.
//
// Delivery is AT-LEAST-ONCE, so a caller MUST dedup by ID — Seq is the cursor, ID is
// the identity, and they are different fields for that reason.
type Event struct {
	Seq       int64        `json:"seq"`
	ID        string       `json:"id"`
	SandboxID string       `json:"sandbox_id"`
	Kind      EventKind    `json:"kind"`
	State     SandboxState `json:"state"`
	// PlacementAddress is the guest-visible address established by a created or
	// resumed event. It is absent on transitions that do not establish a placement.
	PlacementAddress string `json:"placement_address,omitempty"`
	PlacementEpoch   uint64 `json:"placement_epoch,omitempty"`
	// Cause is present only on `destroyed` events, distinguishing an explicit
	// destroy from retention expiry. Crash causes remain on the Sandbox record.
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

// VolumeFileInfo is root-confined metadata returned by Volume content calls.
type VolumeFileInfo struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	Mode          uint32 `json:"mode"`
	IsDir         bool   `json:"is_dir"`
	Type          string `json:"type"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
	ModTime       string `json:"mod_time"`
}

type Measurement struct {
	Available bool  `json:"available"`
	Value     int64 `json:"value"`
}

type ResourceSnapshot struct {
	CPU    Measurement `json:"cpu"`
	Memory Measurement `json:"memory"`
	Disk   Measurement `json:"disk"`
	State  string      `json:"state"`
	At     string      `json:"at"`
}

type EventEnvelope struct {
	ID         string `json:"id"`
	Stream     string `json:"stream"`
	ObjectKind string `json:"object_kind"`
	ObjectID   string `json:"object_id"`
	Epoch      uint64 `json:"epoch,omitempty"`
	Sequence   uint64 `json:"sequence"`
	OccurredAt string `json:"occurred_at"`
}

type TelemetrySample struct {
	Envelope       EventEnvelope    `json:"envelope"`
	SandboxID      string           `json:"sandbox_id"`
	PlacementEpoch uint64           `json:"placement_epoch,omitempty"`
	ProcessID      string           `json:"process_id,omitempty"`
	Snapshot       ResourceSnapshot `json:"snapshot"`
}

type TelemetryPage struct {
	Samples  []TelemetrySample `json:"samples"`
	Earliest uint64            `json:"earliest"`
	Latest   uint64            `json:"latest"`
}

type SandboxSnapshot struct {
	SandboxID      string           `json:"sandbox_id"`
	State          string           `json:"state"`
	Resources      ResourceSnapshot `json:"resources"`
	PlacementEpoch uint64           `json:"placement_epoch,omitempty"`
	At             string           `json:"at"`
}
type PlacementSnapshot struct {
	SandboxID string           `json:"sandbox_id"`
	Epoch     uint64           `json:"epoch"`
	State     string           `json:"state"`
	Resources ResourceSnapshot `json:"resources"`
	At        string           `json:"at"`
}
type ProcessSnapshot struct {
	ProcessID      string           `json:"process_id"`
	SandboxID      string           `json:"sandbox_id"`
	PlacementEpoch uint64           `json:"placement_epoch"`
	State          string           `json:"state"`
	PID            int              `json:"pid,omitempty"`
	ExitCode       *int             `json:"exit_code,omitempty"`
	Resources      ResourceSnapshot `json:"resources"`
	At             string           `json:"at"`
}
type Process struct {
	ProcessID      string  `json:"process_id"`
	SandboxID      string  `json:"sandbox_id"`
	PlacementEpoch uint64  `json:"placement_epoch"`
	PID            int     `json:"pid"`
	State          string  `json:"state"`
	ExitCode       *int    `json:"exit_code,omitempty"`
	StartedAt      string  `json:"started_at"`
	ExitedAt       *string `json:"exited_at,omitempty"`
	OutputCursor   Cursor  `json:"output_cursor"`
}
type Cursor struct {
	Stream   string `json:"stream"`
	ObjectID string `json:"object_id,omitempty"`
	Epoch    uint64 `json:"epoch,omitempty"`
	Sequence uint64 `json:"sequence"`
}
