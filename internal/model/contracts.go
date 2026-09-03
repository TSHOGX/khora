package model

// This file contains the Phase 0 contracts that are shared by the control
// plane and its edges.  The types are deliberately pure: they carry durable
// identity and policy, but perform no I/O and do not depend on a provider,
// transport, or Store implementation.  Later phases may persist or expose
// these values through their own canonical schema.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// PlacementEpoch identifies one realization of a Sandbox.  Zero is the
// absence of a realization; a committed Placement always has a positive
// epoch.  An Adopt keeps the epoch, while a Resume allocates the next one.
type PlacementEpoch uint64

const FirstPlacementEpoch PlacementEpoch = 1

// NextPlacementEpoch allocates the epoch for a cold realization.  It accepts
// zero so the first Resume produces epoch one and refuses wraparound rather
// than silently reusing an old epoch.
func NextPlacementEpoch(previous PlacementEpoch) (PlacementEpoch, error) {
	if previous == ^PlacementEpoch(0) {
		return 0, NewContractError(ErrorInternal, "placement epoch exhausted")
	}
	return previous + 1, nil
}

// AdoptPlacementEpoch returns the epoch for a recovered realization.  Adopt
// is valid only for an existing epoch and must never mint a new one.
func AdoptPlacementEpoch(current PlacementEpoch) (PlacementEpoch, error) {
	if current == 0 {
		return 0, NewContractError(ErrorInvalidArgument, "adopt requires a positive placement epoch")
	}
	return current, nil
}

// PlacementState is the durable state of the current realization.  The
// Sandbox lifecycle remains the authority for desired state; this state says
// what the placement record currently represents.
type PlacementState string

const (
	PlacementActive   PlacementState = "active"
	PlacementStopping PlacementState = "stopping"
	PlacementLost     PlacementState = "lost"
)

var placementStates = [...]PlacementState{
	PlacementActive,
	PlacementStopping,
	PlacementLost,
}

func (s PlacementState) Valid() bool {
	for _, want := range placementStates {
		if s == want {
			return true
		}
	}
	return false
}

// Placement is the durable record for the current Sandbox realization.  A
// provider handle, host path, namespace descriptor, and guest PID do not
// belong here; they are placement-local implementation details.  The
// composite (Sandbox, Epoch) is the placement identity.
type Placement struct {
	Sandbox   SandboxID      `json:"sandbox_id"`
	Epoch     PlacementEpoch `json:"epoch"`
	Provider  string         `json:"provider"`
	State     PlacementState `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func (p Placement) Validate() error {
	if p.Sandbox == "" {
		return NewContractError(ErrorInvalidArgument, "placement sandbox is required")
	}
	if p.Epoch == 0 {
		return NewContractError(ErrorInvalidArgument, "placement epoch must be positive")
	}
	if p.Provider == "" {
		return NewContractError(ErrorInvalidArgument, "placement provider is required")
	}
	if !p.State.Valid() {
		return NewContractError(ErrorInvalidArgument, "unknown placement state")
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) {
		return NewContractError(ErrorInvalidArgument, "placement timestamps are invalid")
	}
	return nil
}

// Matches reports whether an operation addressed this exact Sandbox
// realization.  A different epoch is stale even when the Sandbox ID matches.
func (p Placement) Matches(sandbox SandboxID, epoch PlacementEpoch) bool {
	return p.Sandbox == sandbox && p.Epoch == epoch
}

// ValidateRequest checks both halves of an address.  A missing Sandbox is a
// not-found result; a matching Sandbox with a different epoch is stale.
func (p Placement) ValidateRequest(sandbox SandboxID, epoch PlacementEpoch) error {
	if sandbox == "" || epoch == 0 {
		return NewContractError(ErrorInvalidArgument, "sandbox and placement epoch are required")
	}
	if p.Sandbox != sandbox {
		return NewContractError(ErrorNotFound, "placement sandbox not found")
	}
	return ValidateEpoch(epoch, p.Epoch)
}

// ProcessID is an opaque, durable process handle.  It is intentionally not a
// guest PID; PIDs are only locators inside one Placement epoch.
type ProcessID string

type ProcessState string

const (
	ProcessRunning   ProcessState = "running"
	ProcessExited    ProcessState = "exited"
	ProcessPaused    ProcessState = "paused"
	ProcessReclaimed ProcessState = "reclaimed"
)

var processStates = [...]ProcessState{
	ProcessRunning,
	ProcessExited,
	ProcessPaused,
	ProcessReclaimed,
}

func (s ProcessState) Valid() bool {
	for _, want := range processStates {
		if s == want {
			return true
		}
	}
	return false
}

// Process is the durable logical process record.  PID is retained only as a
// placement-local locator.  ExitCode is present only after an exited result
// has been observed; a missing code is different from a successful zero.
type Process struct {
	ID             ProcessID      `json:"process_id"`
	Sandbox        SandboxID      `json:"sandbox_id"`
	PlacementEpoch PlacementEpoch `json:"placement_epoch"`
	PID            int            `json:"pid"`
	State          ProcessState   `json:"state"`
	ExitCode       *int           `json:"exit_code,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	ExitedAt       *time.Time     `json:"exited_at,omitempty"`
	OutputCursor   Cursor         `json:"output_cursor"`
}

func (p Process) Validate() error {
	if p.ID == "" {
		return NewContractError(ErrorInvalidArgument, "process handle is required")
	}
	if p.Sandbox == "" {
		return NewContractError(ErrorInvalidArgument, "process sandbox is required")
	}
	if p.PlacementEpoch == 0 {
		return NewContractError(ErrorInvalidArgument, "process placement epoch must be positive")
	}
	// A PID is only a locator while the process has a live placement.  Paused
	// and reclaimed records intentionally carry no current locator; retaining a
	// positive historical PID is also allowed for diagnostics, but it is never
	// used as process identity.
	if p.PID < 0 || (p.State == ProcessRunning && p.PID == 0) {
		return NewContractError(ErrorInvalidArgument, "process PID locator is invalid")
	}
	if !p.State.Valid() {
		return NewContractError(ErrorInvalidArgument, "unknown process state")
	}
	if p.StartedAt.IsZero() {
		return NewContractError(ErrorInvalidArgument, "process start time is required")
	}
	if p.ExitedAt != nil && p.ExitedAt.Before(p.StartedAt) {
		return NewContractError(ErrorInvalidArgument, "process exit time precedes start time")
	}
	if p.State == ProcessExited && p.ExitedAt == nil {
		return NewContractError(ErrorInvalidArgument, "exited process requires an exit time")
	}
	if p.State == ProcessRunning && p.ExitedAt != nil {
		return NewContractError(ErrorInvalidArgument, "running process cannot have an exit time")
	}
	if p.State != ProcessExited && p.ExitCode != nil {
		return NewContractError(ErrorInvalidArgument, "exit code is only valid for an exited process")
	}
	if p.OutputCursor.Stream != CursorStreamProcessOutput {
		return NewContractError(ErrorInvalidArgument, "process output cursor has the wrong stream")
	}
	if p.OutputCursor.ObjectID != string(p.ID) || p.OutputCursor.Epoch != p.PlacementEpoch {
		return NewContractError(ErrorInvalidArgument, "process output cursor does not identify the process placement")
	}
	return p.OutputCursor.Validate()
}

// IsStale reports whether a request carrying epoch is for an older or newer
// realization.  The caller maps this to a fail-closed stale-epoch error.
func (p Process) IsStale(epoch PlacementEpoch) bool {
	return epoch != p.PlacementEpoch
}

// ProcessRetentionPolicy controls how long an exited logical process remains
// addressable.  The default is a bounded 24-hour replay/reconnect window.
// Destroying the parent Sandbox always reclaims remaining records immediately.
type ProcessRetentionPolicy struct {
	ExitedFor time.Duration `json:"exited_for"`
}

const (
	DefaultProcessExitRetention time.Duration = 24 * time.Hour
	ProcessReclaimedEvent                     = "process-reclaimed"
)

var DefaultProcessRetention = ProcessRetentionPolicy{ExitedFor: DefaultProcessExitRetention}

func (p ProcessRetentionPolicy) Validate() error {
	if p.ExitedFor <= 0 {
		return NewContractError(ErrorInvalidArgument, "process exit retention must be positive")
	}
	return nil
}

// Reclaimable reports whether an exited process has reached the retention
// boundary.  The boundary is inclusive, making cleanup deterministic at the
// exact deadline.
func (p Process) Reclaimable(now time.Time, policy ProcessRetentionPolicy) bool {
	if p.State != ProcessExited || p.ExitedAt == nil || policy.ExitedFor <= 0 {
		return false
	}
	return !now.Before(p.ExitedAt.Add(policy.ExitedFor))
}

// ManifestSchemaVersion is the typed Template manifest schema number.
type ManifestSchemaVersion uint32

const CurrentManifestSchema ManifestSchemaVersion = 1

type DigestAlgorithm string

const DigestSHA256 DigestAlgorithm = "sha256"

// TemplateManifest declares immutable Template metadata.  ContentDigest is
// optional, but when supplied it is always prefixed with its declared
// algorithm (for example, sha256:<hex>).  Capabilities are normalized as a
// sorted, duplicate-free set before persistence.
type TemplateManifest struct {
	SchemaVersion   ManifestSchemaVersion `json:"schema_version"`
	DigestAlgorithm DigestAlgorithm       `json:"digest_algorithm,omitempty"`
	ContentDigest   string                `json:"content_digest,omitempty"`
	Platform        string                `json:"platform"`
	Capabilities    []string              `json:"capabilities"`
	Provenance      string                `json:"provenance,omitempty"`
	SBOMReference   string                `json:"sbom_reference,omitempty"`
}

func (m TemplateManifest) Normalize() TemplateManifest {
	out := m
	out.Capabilities = append([]string(nil), m.Capabilities...)
	sort.Strings(out.Capabilities)
	if len(out.Capabilities) > 0 {
		uniq := out.Capabilities[:0]
		for _, capability := range out.Capabilities {
			if len(uniq) == 0 || uniq[len(uniq)-1] != capability {
				uniq = append(uniq, capability)
			}
		}
		out.Capabilities = uniq
	} else {
		out.Capabilities = []string{}
	}
	return out
}

func (m TemplateManifest) Validate() error {
	if m.SchemaVersion == 0 || m.SchemaVersion > CurrentManifestSchema {
		return NewContractError(ErrorInvalidArgument, "unsupported Template manifest schema version")
	}
	parts := strings.Split(m.Platform, "/")
	if strings.TrimSpace(m.Platform) != m.Platform || len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return NewContractError(ErrorInvalidArgument, "Template manifest platform must be os/arch")
	}
	if m.Capabilities == nil {
		return NewContractError(ErrorInvalidArgument, "Template manifest capabilities must be an array")
	}
	seen := map[string]struct{}{}
	for _, capability := range m.Capabilities {
		if strings.TrimSpace(capability) == "" || strings.TrimSpace(capability) != capability {
			return NewContractError(ErrorInvalidArgument, "Template manifest capability cannot be empty")
		}
		if _, ok := seen[capability]; ok {
			return NewContractError(ErrorInvalidArgument, "Template manifest capabilities must be unique")
		}
		seen[capability] = struct{}{}
	}
	if (m.DigestAlgorithm == "") != (m.ContentDigest == "") {
		return NewContractError(ErrorInvalidArgument, "Template content digest and digest algorithm must be supplied together")
	}
	if m.DigestAlgorithm != "" && m.DigestAlgorithm != DigestSHA256 {
		return NewContractError(ErrorUnsupported, "unsupported Template digest algorithm")
	}
	if m.ContentDigest != "" {
		prefix := string(DigestSHA256) + ":"
		if !strings.HasPrefix(m.ContentDigest, prefix) || len(m.ContentDigest) != len(prefix)+sha256.Size*2 {
			return NewContractError(ErrorInvalidArgument, "Template content digest is not a sha256 digest")
		}
		digestHex := strings.TrimPrefix(m.ContentDigest, prefix)
		if strings.ToLower(digestHex) != digestHex {
			return NewContractError(ErrorInvalidArgument, "Template content digest must use lowercase hexadecimal")
		}
		if _, err := hex.DecodeString(digestHex); err != nil {
			return NewContractError(ErrorInvalidArgument, "Template content digest is not hexadecimal")
		}
	}
	return nil
}

// DigestBytes computes the canonical SHA-256 content digest spelling used by
// Template manifests.
func DigestBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return string(DigestSHA256) + ":" + hex.EncodeToString(sum[:])
}

// OperationID and IdempotencyKey are opaque references supplied by the
// control plane and its caller respectively.
type OperationID string
type IdempotencyKey string

type OperationState string

const (
	OperationInProgress OperationState = "in_progress"
	OperationSucceeded  OperationState = "succeeded"
	OperationFailed     OperationState = "failed"
)

var operationStates = [...]OperationState{
	OperationInProgress,
	OperationSucceeded,
	OperationFailed,
}

func (s OperationState) Valid() bool {
	for _, want := range operationStates {
		if s == want {
			return true
		}
	}
	return false
}

// OperationPhase classifies every side-effect window.  Recovery can therefore
// choose cleanup versus retry without guessing from an in-memory flag.
type OperationPhase string

const (
	OperationRecorded      OperationPhase = "recorded"
	OperationMaterializing OperationPhase = "materializing"
	OperationMaterialized  OperationPhase = "materialized"
	OperationCommitted     OperationPhase = "committed"
	OperationCleaning      OperationPhase = "cleaning"
)

var operationPhases = [...]OperationPhase{
	OperationRecorded,
	OperationMaterializing,
	OperationMaterialized,
	OperationCommitted,
	OperationCleaning,
}

func (p OperationPhase) Valid() bool {
	for _, want := range operationPhases {
		if p == want {
			return true
		}
	}
	return false
}

type ObjectKind string

const (
	ObjectSandbox ObjectKind = "sandbox"
	ObjectVolume  ObjectKind = "volume"
)

// OperationResult is the durable result replayed for an idempotent retry.
type OperationResult struct {
	Kind ObjectKind `json:"kind"`
	ID   string     `json:"id"`
}

func (r OperationResult) Validate() error {
	if r.Kind != ObjectSandbox && r.Kind != ObjectVolume {
		return NewContractError(ErrorInvalidArgument, "unknown operation result object kind")
	}
	if r.ID == "" {
		return NewContractError(ErrorInvalidArgument, "operation result object ID is required")
	}
	return nil
}

// Operation is the durable idempotency ledger row.  A row is recorded before
// external materialization.  Same key + same request hash replays this row;
// same key + a different hash is a deterministic conflict.
type Operation struct {
	ID             OperationID      `json:"operation_id"`
	Scope          string           `json:"scope"`
	IdempotencyKey IdempotencyKey   `json:"idempotency_key"`
	RequestHash    string           `json:"request_hash"`
	State          OperationState   `json:"state"`
	Phase          OperationPhase   `json:"phase"`
	Attempt        uint32           `json:"attempt"`
	Result         *OperationResult `json:"result,omitempty"`
	ErrorClass     ErrorClass       `json:"error_class,omitempty"`
	StartedAt      time.Time        `json:"started_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	LeaseUntil     time.Time        `json:"lease_until"`
}

const DefaultOperationLease time.Duration = 30 * time.Second

// BeginOperation constructs the row that must be committed before an
// external side effect. It starts in the recorded phase so restart recovery
// can distinguish "no side effect began" from a materialization that needs
// cleanup.
func BeginOperation(id OperationID, scope string, key IdempotencyKey, requestHash string, now time.Time, lease time.Duration) (Operation, error) {
	if strings.TrimSpace(string(id)) == "" || strings.TrimSpace(scope) == "" || strings.TrimSpace(string(key)) == "" || requestHash == "" {
		return Operation{}, NewContractError(ErrorInvalidArgument, "operation identity and request hash are required")
	}
	if lease <= 0 {
		return Operation{}, NewContractError(ErrorInvalidArgument, "operation lease must be positive")
	}
	if now.IsZero() {
		return Operation{}, NewContractError(ErrorInvalidArgument, "operation start time is required")
	}
	op := Operation{
		ID:             id,
		Scope:          scope,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		State:          OperationInProgress,
		Phase:          OperationRecorded,
		Attempt:        1,
		StartedAt:      now,
		UpdatedAt:      now,
		LeaseUntil:     now.Add(lease),
	}
	if err := op.Validate(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func (o Operation) Validate() error {
	if strings.TrimSpace(string(o.ID)) == "" || strings.TrimSpace(o.Scope) == "" || strings.TrimSpace(string(o.IdempotencyKey)) == "" || o.RequestHash == "" {
		return NewContractError(ErrorInvalidArgument, "operation identity and request hash are required")
	}
	if !strings.HasPrefix(o.RequestHash, string(DigestSHA256)+":") || len(o.RequestHash) != len(string(DigestSHA256))+1+sha256.Size*2 {
		return NewContractError(ErrorInvalidArgument, "operation request hash is not a sha256 digest")
	}
	requestHex := strings.TrimPrefix(o.RequestHash, string(DigestSHA256)+":")
	if strings.ToLower(requestHex) != requestHex {
		return NewContractError(ErrorInvalidArgument, "operation request hash must use lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(requestHex); err != nil {
		return NewContractError(ErrorInvalidArgument, "operation request hash is not hexadecimal")
	}
	if !o.State.Valid() || !o.Phase.Valid() {
		return NewContractError(ErrorInvalidArgument, "unknown operation state or phase")
	}
	if !o.ErrorClass.Valid() {
		return NewContractError(ErrorInvalidArgument, "unknown operation error class")
	}
	if o.Attempt == 0 {
		return NewContractError(ErrorInvalidArgument, "operation attempt must be positive")
	}
	if o.StartedAt.IsZero() || o.UpdatedAt.IsZero() || o.UpdatedAt.Before(o.StartedAt) {
		return NewContractError(ErrorInvalidArgument, "operation timestamps are invalid")
	}
	if o.State == OperationInProgress && o.LeaseUntil.IsZero() {
		return NewContractError(ErrorInvalidArgument, "in-progress operation requires a lease")
	}
	if o.State == OperationInProgress && o.Phase == OperationCommitted {
		return NewContractError(ErrorInvalidArgument, "in-progress operation cannot be in the committed phase")
	}
	if o.State != OperationInProgress && !o.LeaseUntil.IsZero() {
		return NewContractError(ErrorInvalidArgument, "terminal operation cannot retain a lease")
	}
	if o.State == OperationInProgress {
		if o.ErrorClass != ErrorNone || o.Result != nil {
			return NewContractError(ErrorInvalidArgument, "in-progress operation cannot carry a terminal result")
		}
	}
	if o.State == OperationSucceeded {
		if o.Phase != OperationCommitted || o.Result == nil || o.ErrorClass != ErrorNone {
			return NewContractError(ErrorInvalidArgument, "succeeded operation requires a committed result")
		}
		if err := o.Result.Validate(); err != nil {
			return err
		}
	}
	if o.State == OperationFailed {
		if o.ErrorClass == ErrorNone || o.Phase != OperationCommitted || o.Result != nil {
			return NewContractError(ErrorInvalidArgument, "failed operation requires a terminal error class")
		}
	}
	return nil
}

// RequestHash computes the canonical hash used by the idempotency ledger.
// encoding/json gives map keys a stable order, and the resulting SHA-256
// spelling is explicit so all edges can compare it without sharing a type.
func RequestHash(request any) (string, error) {
	if request == nil {
		return "", NewContractError(ErrorInvalidArgument, "operation request cannot be nil")
	}
	b, err := json.Marshal(request)
	if err != nil {
		return "", NewContractError(ErrorInvalidArgument, "operation request is not JSON encodable")
	}
	return DigestBytes(b), nil
}

// SameRequest implements the key-reuse rule.
func (o Operation) SameRequest(hash string) error {
	if hash == "" {
		return NewContractError(ErrorInvalidArgument, "operation request hash is required")
	}
	if o.RequestHash != hash {
		return NewContractError(ErrorIdempotencyConflict, "idempotency key was reused with a different request")
	}
	return nil
}

// Advance records progress through the side-effect windows. Terminal phases
// cannot be revisited, and an in-progress operation cannot move backwards.
func (o *Operation) Advance(phase OperationPhase, now time.Time) error {
	if o == nil {
		return NewContractError(ErrorInvalidArgument, "operation is nil")
	}
	if o.State != OperationInProgress {
		return NewContractError(ErrorConflict, "operation is already terminal")
	}
	if !phase.Valid() || phase == OperationCommitted {
		return NewContractError(ErrorInvalidArgument, "invalid in-progress operation phase")
	}
	if now.IsZero() || now.Before(o.UpdatedAt) {
		return NewContractError(ErrorInvalidArgument, "operation update time is invalid")
	}
	order := map[OperationPhase]int{
		OperationRecorded:      0,
		OperationMaterializing: 1,
		OperationMaterialized:  2,
		OperationCleaning:      3,
	}
	if phase != OperationCleaning && order[phase] < order[o.Phase] {
		return NewContractError(ErrorConflict, "operation phase cannot move backwards")
	}
	o.Phase = phase
	o.UpdatedAt = now
	return nil
}

// Succeed commits an idempotent result. Once terminal, the result is
// immutable and retries replay it instead of repeating external work.
func (o *Operation) Succeed(result OperationResult, now time.Time) error {
	if o == nil {
		return NewContractError(ErrorInvalidArgument, "operation is nil")
	}
	if o.State != OperationInProgress || o.Phase == OperationCleaning {
		return NewContractError(ErrorConflict, "operation cannot be committed from its current state")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	if now.IsZero() || now.Before(o.UpdatedAt) {
		return NewContractError(ErrorInvalidArgument, "operation update time is invalid")
	}
	o.State = OperationSucceeded
	o.Phase = OperationCommitted
	o.Result = &result
	o.ErrorClass = ErrorNone
	o.UpdatedAt = now
	o.LeaseUntil = time.Time{}
	return nil
}

// Fail commits a terminal error. Error messages remain outside the durable
// contract; only the stable class is retained for deterministic retries.
func (o *Operation) Fail(class ErrorClass, now time.Time) error {
	if o == nil {
		return NewContractError(ErrorInvalidArgument, "operation is nil")
	}
	if o.State != OperationInProgress || o.Phase == OperationCleaning {
		return NewContractError(ErrorConflict, "operation cannot be failed from its current state")
	}
	if !class.Valid() || class == ErrorNone {
		return NewContractError(ErrorInvalidArgument, "operation failure requires a valid error class")
	}
	if now.IsZero() || now.Before(o.UpdatedAt) {
		return NewContractError(ErrorInvalidArgument, "operation update time is invalid")
	}
	o.State = OperationFailed
	o.Phase = OperationCommitted
	o.ErrorClass = class
	o.Result = nil
	o.UpdatedAt = now
	o.LeaseUntil = time.Time{}
	return nil
}

// TakeoverAllowed reports whether a restart may take over an in-progress row.
func (o Operation) TakeoverAllowed(now time.Time) bool {
	return o.State == OperationInProgress && !o.LeaseUntil.IsZero() && !now.Before(o.LeaseUntil)
}

// Takeover advances an expired in-progress row.  It does not alter the
// idempotency key or request hash, so retries still converge on one row.
func (o *Operation) Takeover(now time.Time, lease time.Duration) error {
	if o == nil {
		return NewContractError(ErrorInvalidArgument, "operation is nil")
	}
	if lease <= 0 {
		return NewContractError(ErrorInvalidArgument, "operation takeover lease must be positive")
	}
	if now.IsZero() || now.Before(o.UpdatedAt) {
		return NewContractError(ErrorInvalidArgument, "operation takeover time is invalid")
	}
	if !o.TakeoverAllowed(now) {
		return NewContractError(ErrorInProgress, "operation is still owned by another attempt")
	}
	if o.Phase == OperationCommitted {
		return NewContractError(ErrorInvalidArgument, "committed operation cannot be taken over")
	}
	if o.Attempt == ^uint32(0) {
		return NewContractError(ErrorInternal, "operation attempt exhausted")
	}
	o.Attempt++
	// Preserve the side-effect phase so restart reconciliation still knows
	// whether cleanup is required before retrying.
	o.UpdatedAt = now
	o.LeaseUntil = now.Add(lease)
	return nil
}

// RecoveryAction is the deterministic action for an in-progress operation
// found during restart reconciliation.
type RecoveryAction string

const (
	RecoveryRetry   RecoveryAction = "retry"
	RecoveryCleanup RecoveryAction = "cleanup_then_retry"
	RecoveryVerify  RecoveryAction = "verify_and_replay"
)

func (o Operation) RecoveryAction() RecoveryAction {
	switch o.Phase {
	case OperationRecorded:
		return RecoveryRetry
	case OperationMaterializing, OperationMaterialized, OperationCleaning:
		return RecoveryCleanup
	case OperationCommitted:
		return RecoveryVerify
	default:
		return RecoveryCleanup
	}
}

// CursorStream identifies an independent ordered stream.  Lifecycle events,
// process output, filesystem watch events, and telemetry samples never share a
// sequence or cursor.
type CursorStream string

const (
	CursorStreamLifecycle     CursorStream = "lifecycle"
	CursorStreamProcessOutput CursorStream = "process-output"
	CursorStreamWatch         CursorStream = "watch"
	CursorStreamTelemetry     CursorStream = "telemetry"
)

var cursorStreams = [...]CursorStream{
	CursorStreamLifecycle,
	CursorStreamProcessOutput,
	CursorStreamWatch,
	CursorStreamTelemetry,
}

// Cursor is a portable replay position.  Sequence zero is the initial
// position.  Placement-scoped streams carry a positive epoch; lifecycle and
// telemetry streams are independent of a Placement epoch.
type Cursor struct {
	Stream   CursorStream   `json:"stream"`
	ObjectID string         `json:"object_id,omitempty"`
	Epoch    PlacementEpoch `json:"epoch,omitempty"`
	Sequence uint64         `json:"sequence"`
}

func LifecycleCursor(sequence uint64) Cursor {
	return Cursor{Stream: CursorStreamLifecycle, Sequence: sequence}
}

func ProcessOutputCursor(processID ProcessID, epoch PlacementEpoch, sequence uint64) Cursor {
	return Cursor{Stream: CursorStreamProcessOutput, ObjectID: string(processID), Epoch: epoch, Sequence: sequence}
}

func WatchCursor(sandboxID SandboxID, epoch PlacementEpoch, sequence uint64) Cursor {
	return Cursor{Stream: CursorStreamWatch, ObjectID: string(sandboxID), Epoch: epoch, Sequence: sequence}
}

func TelemetryCursor(objectID string, sequence uint64) Cursor {
	return Cursor{Stream: CursorStreamTelemetry, ObjectID: objectID, Sequence: sequence}
}

func (c Cursor) Validate() error {
	validStream := false
	for _, stream := range cursorStreams {
		if c.Stream == stream {
			validStream = true
			break
		}
	}
	if !validStream {
		return NewContractError(ErrorInvalidArgument, "unknown cursor stream")
	}
	switch c.Stream {
	case CursorStreamLifecycle:
		if c.ObjectID != "" || c.Epoch != 0 {
			return NewContractError(ErrorInvalidArgument, "lifecycle cursor cannot carry an object or epoch")
		}
	case CursorStreamProcessOutput, CursorStreamWatch:
		if c.ObjectID == "" || c.Epoch == 0 {
			return NewContractError(ErrorInvalidArgument, "placement cursor requires an object and positive epoch")
		}
	case CursorStreamTelemetry:
		if c.ObjectID == "" || c.Epoch != 0 {
			return NewContractError(ErrorInvalidArgument, "telemetry cursor requires an object and no placement epoch")
		}
	}
	return nil
}

const CursorEncodingVersion = "v1"

// Encode returns the URL-safe, versioned cursor spelling used at API edges.
func (c Cursor) Encode() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", NewContractError(ErrorInternal, "encode cursor: "+err.Error())
	}
	return CursorEncodingVersion + "." + base64.RawURLEncoding.EncodeToString(b), nil
}

func ParseCursor(raw string) (Cursor, error) {
	prefix := CursorEncodingVersion + "."
	if !strings.HasPrefix(raw, prefix) {
		return Cursor{}, NewContractError(ErrorInvalidArgument, "unsupported cursor encoding")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		return Cursor{}, NewContractError(ErrorInvalidArgument, "malformed cursor encoding")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var c Cursor
	if err := dec.Decode(&c); err != nil {
		return Cursor{}, NewContractError(ErrorInvalidArgument, "malformed cursor payload")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Cursor{}, NewContractError(ErrorInvalidArgument, "cursor payload has trailing data")
	}
	if err := c.Validate(); err != nil {
		return Cursor{}, err
	}
	return c, nil
}

func (c Cursor) sameStream(other Cursor) bool {
	return c.Stream == other.Stream && c.ObjectID == other.ObjectID && c.Epoch == other.Epoch
}

// Compare orders cursors from one stream.  Comparing different streams is a
// contract error rather than an arbitrary lexical order.
func (c Cursor) Compare(other Cursor) (int, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if err := other.Validate(); err != nil {
		return 0, err
	}
	if !c.sameStream(other) {
		return 0, NewContractError(ErrorInvalidArgument, "cursors belong to different streams")
	}
	switch {
	case c.Sequence < other.Sequence:
		return -1, nil
	case c.Sequence > other.Sequence:
		return 1, nil
	default:
		return 0, nil
	}
}

type CursorResetReason string

const (
	CursorResetPlacement CursorResetReason = "placement-reset"
	CursorResetOverflow  CursorResetReason = "overflow"
	CursorResetExpired   CursorResetReason = "retention-expired"
)

func (r CursorResetReason) Valid() bool {
	return r == CursorResetPlacement || r == CursorResetOverflow || r == CursorResetExpired
}

// CursorReset is explicit control information.  A reset never masquerades as
// an empty page: callers must rescan when Rescan is true.
type CursorReset struct {
	Reason CursorResetReason `json:"reason"`
	Cursor Cursor            `json:"cursor"`
	Rescan bool              `json:"rescan"`
}

// CursorWindow describes the retained sequence bounds for one stream.
type CursorWindow struct {
	Stream   CursorStream   `json:"stream"`
	ObjectID string         `json:"object_id,omitempty"`
	Epoch    PlacementEpoch `json:"epoch,omitempty"`
	Earliest uint64         `json:"earliest"`
	Latest   uint64         `json:"latest"`
	Overflow bool           `json:"overflow"`
	Reset    *CursorReset   `json:"reset,omitempty"`
}

// NewCursorWindow binds retained bounds to one stream scope.  The scope is
// part of the contract so a cursor from another process, Sandbox, or epoch
// cannot be accidentally accepted.
func NewCursorWindow(scope Cursor, earliest, latest uint64) (CursorWindow, error) {
	if err := scope.Validate(); err != nil {
		return CursorWindow{}, err
	}
	if earliest == 0 && latest != 0 {
		return CursorWindow{}, NewContractError(ErrorInvalidArgument, "cursor window latest requires an earliest bound")
	}
	if earliest != 0 && latest == 0 {
		return CursorWindow{}, NewContractError(ErrorInvalidArgument, "cursor window earliest requires a latest bound")
	}
	if latest != 0 && latest < earliest {
		return CursorWindow{}, NewContractError(ErrorInvalidArgument, "cursor window bounds are reversed")
	}
	return CursorWindow{Stream: scope.Stream, ObjectID: scope.ObjectID, Epoch: scope.Epoch, Earliest: earliest, Latest: latest}, nil
}

func (w CursorWindow) Validate() error {
	if w.Stream == "" {
		return NewContractError(ErrorInvalidArgument, "cursor window stream is required")
	}
	scope := Cursor{Stream: w.Stream, ObjectID: w.ObjectID, Epoch: w.Epoch}
	if err := scope.Validate(); err != nil {
		return err
	}
	if w.Earliest == 0 && w.Latest != 0 {
		return NewContractError(ErrorInvalidArgument, "cursor window latest requires an earliest bound")
	}
	if w.Earliest != 0 && w.Latest == 0 {
		return NewContractError(ErrorInvalidArgument, "cursor window earliest requires a latest bound")
	}
	if w.Latest != 0 && w.Latest < w.Earliest {
		return NewContractError(ErrorInvalidArgument, "cursor window bounds are reversed")
	}
	if w.Reset != nil {
		if !w.Reset.Reason.Valid() {
			return NewContractError(ErrorInvalidArgument, "unknown cursor reset reason")
		}
		if err := w.Reset.Cursor.Validate(); err != nil {
			return err
		}
		if w.Reset.Cursor.Stream != w.Stream || w.Reset.Cursor.ObjectID != w.ObjectID || w.Reset.Cursor.Epoch != w.Epoch {
			return NewContractError(ErrorInvalidArgument, "cursor reset does not identify this stream")
		}
		if w.Reset.Cursor.Sequence != 0 {
			return NewContractError(ErrorInvalidArgument, "cursor reset must start at sequence zero")
		}
		if w.Reset.Reason == CursorResetPlacement && w.Stream != CursorStreamProcessOutput && w.Stream != CursorStreamWatch {
			return NewContractError(ErrorInvalidArgument, "placement reset is only valid for placement streams")
		}
		if w.Reset.Reason == CursorResetOverflow && w.Stream == CursorStreamLifecycle {
			return NewContractError(ErrorInvalidArgument, "overflow reset is not valid for the lifecycle stream")
		}
		if !w.Reset.Rescan {
			return NewContractError(ErrorInvalidArgument, "cursor reset must require a rescan")
		}
	}
	return nil
}

// Accept checks whether after can safely resume this window.  Initial cursor
// zero is always accepted.  A placement epoch mismatch is stale, an omitted
// retained range is an explicit expiry, and an overflow requires a rescan.
func (w CursorWindow) Accept(after Cursor) error {
	if err := w.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if after.Stream != w.Stream || after.ObjectID != w.ObjectID {
		return NewContractError(ErrorInvalidArgument, "cursor does not belong to this stream")
	}
	if after.Epoch != w.Epoch {
		return NewContractError(ErrorStaleEpoch, "cursor belongs to a different placement epoch")
	}
	if w.Reset != nil {
		switch w.Reset.Reason {
		case CursorResetPlacement:
			return NewContractError(ErrorStaleEpoch, "cursor requires the new placement epoch")
		case CursorResetOverflow:
			return NewContractError(ErrorCursorGap, "cursor requires a full rescan after overflow")
		case CursorResetExpired:
			return NewContractError(ErrorCursorExpired, "cursor requires resynchronization after retention expiry")
		}
	}
	if w.Overflow {
		return NewContractError(ErrorCursorGap, "cursor fell behind a bounded stream buffer; rescan required")
	}
	if after.Sequence == 0 {
		return nil
	}
	if w.Latest == 0 {
		return NewContractError(ErrorCursorExpired, "cursor is not valid for an empty retained window")
	}
	if w.Earliest > 1 && after.Sequence < w.Earliest-1 {
		return NewContractError(ErrorCursorExpired, "cursor is older than the retained window")
	}
	return nil
}

// PlacementReset returns a reset marker for a new placement epoch.  The
// sequence starts at zero because output/watch state is not carried across a
// pause/resume boundary.
func PlacementReset(stream CursorStream, objectID string, epoch PlacementEpoch) (CursorReset, error) {
	var c Cursor
	switch stream {
	case CursorStreamProcessOutput:
		c = ProcessOutputCursor(ProcessID(objectID), epoch, 0)
	case CursorStreamWatch:
		c = WatchCursor(SandboxID(objectID), epoch, 0)
	default:
		return CursorReset{}, NewContractError(ErrorInvalidArgument, "placement reset requires process-output or watch stream")
	}
	if err := c.Validate(); err != nil {
		return CursorReset{}, err
	}
	return CursorReset{Reason: CursorResetPlacement, Cursor: c, Rescan: true}, nil
}

// Measurement distinguishes a measured zero from an unavailable reading.
// Value is meaningful only when Available is true; unavailable values are
// represented with Available=false and Value=0.
type Measurement struct {
	Available bool  `json:"available"`
	Value     int64 `json:"value"`
}

func Measured(value int64) Measurement {
	return Measurement{Available: true, Value: value}
}

func UnavailableMeasurement() Measurement {
	return Measurement{}
}

func (m Measurement) Validate() error {
	if m.Value < 0 {
		return NewContractError(ErrorInvalidArgument, "measurement value cannot be negative")
	}
	if !m.Available && m.Value != 0 {
		return NewContractError(ErrorInvalidArgument, "unavailable measurement cannot carry a value")
	}
	return nil
}

// WatchPolicy records the conservative Phase 0 watch contract.  The stream
// reports change detection, not syscall-complete history; overflow is an
// explicit gap that requires a full rescan.
type WatchSemantics string

const WatchChangeDetection WatchSemantics = "change-detection"

type WatchPolicy struct {
	Semantics    WatchSemantics `json:"semantics"`
	BufferEvents uint32         `json:"buffer_events"`
	Recursive    bool           `json:"recursive"`
	ResetOnEpoch bool           `json:"reset_on_epoch"`
}

const DefaultWatchBufferEvents uint32 = 4096

var DefaultWatchPolicy = WatchPolicy{
	Semantics:    WatchChangeDetection,
	BufferEvents: DefaultWatchBufferEvents,
	Recursive:    true,
	ResetOnEpoch: true,
}

func (p WatchPolicy) Validate() error {
	if p.Semantics != WatchChangeDetection {
		return NewContractError(ErrorUnsupported, "unsupported filesystem watch semantics")
	}
	if p.BufferEvents == 0 {
		return NewContractError(ErrorInvalidArgument, "filesystem watch buffer must be positive")
	}
	if !p.ResetOnEpoch {
		return NewContractError(ErrorInvalidArgument, "filesystem watch must reset on placement epoch change")
	}
	return nil
}

// TelemetryPolicy controls the independent sample lane.  Samples are
// timestamped and cursorable per object; lifecycle publication never waits on
// this lane.
type TelemetryPolicy struct {
	Interval  time.Duration `json:"interval"`
	Retention time.Duration `json:"retention"`
}

const (
	DefaultTelemetryInterval  time.Duration = 10 * time.Second
	DefaultTelemetryRetention time.Duration = 24 * time.Hour
)

var DefaultTelemetryPolicy = TelemetryPolicy{
	Interval:  DefaultTelemetryInterval,
	Retention: DefaultTelemetryRetention,
}

func (p TelemetryPolicy) Validate() error {
	if p.Interval <= 0 || p.Retention <= 0 {
		return NewContractError(ErrorInvalidArgument, "telemetry interval and retention must be positive")
	}
	if p.Retention < p.Interval {
		return NewContractError(ErrorInvalidArgument, "telemetry retention must cover at least one sample interval")
	}
	return nil
}

// ErrorClass is the protocol-neutral error vocabulary.  Protocol edges map
// these classes to their own status/code systems; message text is diagnostic
// and never the compatibility contract.
type ErrorClass string

const (
	ErrorNone                ErrorClass = ""
	ErrorInvalidArgument     ErrorClass = "invalid_argument"
	ErrorNotFound            ErrorClass = "not_found"
	ErrorConflict            ErrorClass = "conflict"
	ErrorIdempotencyConflict ErrorClass = "idempotency_conflict"
	ErrorStaleEpoch          ErrorClass = "stale_epoch"
	ErrorUnavailable         ErrorClass = "unavailable"
	ErrorCursorExpired       ErrorClass = "cursor_expired"
	ErrorCursorGap           ErrorClass = "cursor_gap"
	ErrorInProgress          ErrorClass = "in_progress"
	ErrorUnsupported         ErrorClass = "unsupported"
	ErrorInternal            ErrorClass = "internal"
)

var errorClasses = [...]ErrorClass{
	ErrorInvalidArgument,
	ErrorNotFound,
	ErrorConflict,
	ErrorIdempotencyConflict,
	ErrorStaleEpoch,
	ErrorUnavailable,
	ErrorCursorExpired,
	ErrorCursorGap,
	ErrorInProgress,
	ErrorUnsupported,
	ErrorInternal,
}

func (c ErrorClass) Valid() bool {
	if c == ErrorNone {
		return true
	}
	for _, want := range errorClasses {
		if c == want {
			return true
		}
	}
	return false
}

// ContractError carries one stable class and optional human diagnostics.
// Is compares classes, allowing errors.Is to survive contextual wrapping.
type ContractError struct {
	Class   ErrorClass
	Message string
}

func NewContractError(class ErrorClass, message string) *ContractError {
	return &ContractError{Class: class, Message: message}
}

func (e *ContractError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Class)
	}
	return e.Message
}

func (e *ContractError) Is(target error) bool {
	t, ok := target.(*ContractError)
	return ok && e != nil && t != nil && e.Class == t.Class
}

func ErrorClassOf(err error) ErrorClass {
	if err == nil {
		return ErrorNone
	}
	var ce *ContractError
	if errors.As(err, &ce) && ce != nil {
		return ce.Class
	}
	return ErrorInternal
}

var (
	ErrInvalidArgument     = NewContractError(ErrorInvalidArgument, "invalid argument")
	ErrNotFound            = NewContractError(ErrorNotFound, "not found")
	ErrConflict            = NewContractError(ErrorConflict, "conflict")
	ErrIdempotencyConflict = NewContractError(ErrorIdempotencyConflict, "idempotency key conflict")
	ErrStaleEpoch          = NewContractError(ErrorStaleEpoch, "stale placement epoch")
	ErrUnavailable         = NewContractError(ErrorUnavailable, "unavailable")
	ErrCursorExpired       = NewContractError(ErrorCursorExpired, "cursor expired")
	ErrCursorGap           = NewContractError(ErrorCursorGap, "cursor gap")
	ErrInProgress          = NewContractError(ErrorInProgress, "operation in progress")
	ErrUnsupported         = NewContractError(ErrorUnsupported, "unsupported")
	ErrInternal            = NewContractError(ErrorInternal, "internal error")
)

// ValidateEpoch returns a typed stale-epoch error instead of allowing a
// request to act on a different realization.
func ValidateEpoch(expected, actual PlacementEpoch) error {
	if expected == 0 || actual == 0 {
		return NewContractError(ErrorInvalidArgument, "placement epoch must be positive")
	}
	if expected != actual {
		return NewContractError(ErrorStaleEpoch, fmt.Sprintf("placement epoch %d is stale; current epoch is %d", expected, actual))
	}
	return nil
}
