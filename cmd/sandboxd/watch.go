package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// Filesystem watch is placement-scoped.  A transport child can be restarted
// without replacing the placement, so the cursor and the last observed
// snapshots live in a small guest-local journal rather than in one RPC
// handler.  The source remains change detection (not syscall-complete history),
// but reconnects can replay the retained, ordered change records.
const (
	watchBufferEvents     = 4096
	watchPollInterval     = 100 * time.Millisecond
	watchScopeIdle        = 10 * time.Minute
	watchStateFile        = "/tmp/.sandboxd-watch.json"
	watchSubscriberWindow = 256
	// A placement can have many independent path/recursive subscriptions. Keep
	// their retained baselines bounded too; inactive scopes are evicted below.
	watchMaxScopes = 64
)

type watchEventRecord struct {
	Scope string         `json:"scope"`
	Event *v1.WatchEvent `json:"event"`
}

// watchNotice is deliberately separate from WatchEvent.  A terminal reset or
// overflow must still be delivered when a subscriber queue was already full;
// keeping it outside the queue prevents a full queue from hiding the recovery
// instruction.
type watchNotice struct {
	Event *v1.WatchEvent
}

type watchSubscriber struct {
	scope       string
	ch          chan watchNotice
	terminal    *watchNotice
	terminalErr error
	mu          sync.Mutex
}

type watchScope struct {
	key       string
	root      string
	recursive bool
	previous  map[string]watchEntry

	pollMu  sync.Mutex
	running bool
	active  int
	lastUse time.Time
	epoch   uint64
}

// watchHub is the placement-local source and retained cursor.  It owns no
// provider handle and exposes only guest paths/events to the fs service.
type watchHub struct {
	mu sync.Mutex

	path string
	// loadErr is immutable after construction. A journal read or decode failure
	// must not be treated as an empty history: doing so can accept a caller's
	// old cursor against a reset sequence and silently skip retained changes.
	loadErr error
	// journalErr records a persistence failure after construction. Once a write
	// cannot be durably committed, continuing to serve the in-memory cursor could
	// acknowledge events that disappear on restart; fail closed until a fresh
	// placement/transport restart can reload a valid journal.
	journalErr error
	capacity   int
	epoch      uint64
	sequence   uint64
	// overflow is sticky for this placement. Sequence exhaustion is an explicit
	// cursor gap; it must never wrap and silently reuse an old position.
	overflow bool
	records  []watchEventRecord
	// dropped records are tracked per scope.  A global ring can evict records
	// for another scope without making this scope's cursor appear to have a
	// gap, so the eviction watermark is scoped as well.
	dropped map[string]uint64
	scopes  map[string]*watchScope
	subs    map[*watchSubscriber]struct{}
}

type persistedWatchEntry struct {
	Path   string       `json:"path"`
	Type   v1.EntryType `json:"type"`
	Size   int64        `json:"size"`
	MTime  int64        `json:"mtime"`
	Mode   uint32       `json:"mode"`
	Inode  uint64       `json:"inode,omitempty"`
	Device uint64       `json:"device,omitempty"`
}

type persistedWatchScope struct {
	Key       string                         `json:"key"`
	Root      string                         `json:"root"`
	Recursive bool                           `json:"recursive"`
	Epoch     uint64                         `json:"epoch"`
	Previous  map[string]persistedWatchEntry `json:"previous"`
}

type persistedWatchState struct {
	Epoch    uint64                `json:"epoch"`
	Sequence uint64                `json:"sequence"`
	Overflow bool                  `json:"overflow,omitempty"`
	Records  []watchEventRecord    `json:"records"`
	Dropped  map[string]uint64     `json:"dropped,omitempty"`
	Scopes   []persistedWatchScope `json:"scopes,omitempty"`
}

func newWatchHub(path string, capacity int) *watchHub {
	if capacity <= 0 {
		capacity = watchBufferEvents
	}
	h := &watchHub{
		path: path, capacity: capacity,
		dropped: make(map[string]uint64),
		scopes:  make(map[string]*watchScope),
		subs:    make(map[*watchSubscriber]struct{}),
	}
	if path != "" {
		h.loadErr = h.load()
	}
	return h
}

// journalFailure converts an internal persistence failure into a stable wire
// error. The journal path is deliberately omitted: it is a guest-private
// implementation detail and must not cross the transport boundary.
func (h *watchHub) journalFailure() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	failed := h.loadErr != nil || h.journalErr != nil
	h.mu.Unlock()
	if !failed {
		return nil
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("filesystem watch journal unavailable"))
}

func watchScopeKey(root string, recursive bool) string {
	root = filepath.Clean(root)
	if root == "." || root == "" {
		root = "/"
	}
	return fmt.Sprintf("%s\x00%t", root, recursive)
}

func watchEntryPersisted(e watchEntry) persistedWatchEntry {
	return persistedWatchEntry{Path: e.path, Type: e.type_, Size: e.size, MTime: e.mtime, Mode: e.mode, Inode: e.ino, Device: e.dev}
}

func watchEntryRestored(e persistedWatchEntry) watchEntry {
	return watchEntry{path: e.Path, type_: e.Type, size: e.Size, mtime: e.MTime, mode: e.Mode, ino: e.Inode, dev: e.Device}
}

func (h *watchHub) load() error {
	b, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedWatchState
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("watch journal has trailing JSON values")
		}
		return err
	}
	if state.Epoch == 0 {
		return errors.New("watch journal epoch is missing or zero")
	}
	if state.Overflow && state.Sequence != ^uint64(0) {
		return errors.New("watch journal overflow marker does not identify an exhausted sequence")
	}
	// A placement that exhausted its sequence space may have no retained
	// records (for example, the first change arrived at MaxUint64). The sticky
	// overflow bit is then the durable high-water marker; it is the one legal
	// exception to the usual sequence/record-presence pairing.
	if !state.Overflow && (state.Sequence == 0) != (len(state.Records) == 0) {
		return errors.New("watch journal sequence watermark does not match record presence")
	}
	if len(state.Scopes) > watchMaxScopes {
		return errors.New("watch journal contains too many scopes")
	}
	type scopeShape struct {
		root      string
		recursive bool
	}
	scopeShapes := make(map[string]scopeShape, len(state.Scopes))
	seenScopes := make(map[string]struct{}, len(state.Scopes))
	for _, saved := range state.Scopes {
		if saved.Key == "" || saved.Epoch != state.Epoch || saved.Root == "" {
			return errors.New("watch journal contains an invalid scope")
		}
		// Scope identity is derived from the canonical root and recursive bit at
		// subscription time. A journal that carries a different spelling or key
		// would restore a baseline that no future request can address, silently
		// dropping change detection after restart; reject it instead of treating
		// the scope as an empty/new baseline.
		if !filepath.IsAbs(saved.Root) || filepath.Clean(saved.Root) != saved.Root {
			return errors.New("watch journal contains a non-canonical scope root")
		}
		if saved.Key != watchScopeKey(saved.Root, saved.Recursive) {
			return errors.New("watch journal scope key does not match its root")
		}
		if _, ok := seenScopes[saved.Key]; ok {
			return errors.New("watch journal contains duplicate scopes")
		}
		seenScopes[saved.Key] = struct{}{}
		// The writer always serializes an initialized baseline map (an empty
		// baseline is `{}`, not a missing value). A nil map therefore indicates a
		// truncated or hand-edited journal and cannot safely distinguish "empty"
		// from "baseline unavailable" during replay.
		if saved.Previous == nil {
			return errors.New("watch journal scope snapshot is missing")
		}
		shape := scopeShape{root: saved.Root, recursive: saved.Recursive}
		scopeShapes[saved.Key] = shape
		for path, entry := range saved.Previous {
			if err := validatePersistedWatchEntry(path, entry, shape.root, shape.recursive); err != nil {
				return err
			}
		}
	}
	for scope, seq := range state.Dropped {
		if scope == "" || seq == 0 || seq > state.Sequence {
			return errors.New("watch journal has an invalid dropped watermark")
		}
		if _, ok := scopeShapes[scope]; !ok {
			if _, _, ok := parseWatchScopeKey(scope); !ok {
				return errors.New("watch journal dropped watermark names an invalid scope")
			}
		}
	}
	lastSequence := uint64(0)
	for _, rec := range state.Records {
		if rec.Event == nil || rec.Scope == "" {
			return errors.New("watch journal contains an incomplete record")
		}
		if rec.Event.Sequence == 0 || rec.Event.PlacementEpoch != state.Epoch {
			return errors.New("watch journal record has an invalid epoch or sequence")
		}
		if rec.Event.Sequence <= lastSequence {
			return errors.New("watch journal record sequence is not strictly increasing")
		}
		if rec.Event.Sequence > state.Sequence {
			return errors.New("watch journal record exceeds its sequence watermark")
		}
		shape, ok := scopeShapes[rec.Scope]
		if !ok {
			var parsed bool
			shape.root, shape.recursive, parsed = parseWatchScopeKey(rec.Scope)
			if !parsed {
				return errors.New("watch journal record names an invalid scope")
			}
		}
		if err := validateWatchJournalEvent(rec.Event, shape.root, shape.recursive); err != nil {
			return err
		}
		lastSequence = rec.Event.Sequence
	}
	// The ring may evict older records, but it always retains the newest record;
	// therefore a non-empty journal must end at the global high-water mark. A
	// missing newest record would make replay silently skip a change after
	// restart, so reject that corruption rather than resetting history.
	if len(state.Records) != 0 && !state.Overflow && lastSequence != state.Sequence {
		return errors.New("watch journal sequence watermark does not match its records")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epoch, h.sequence, h.overflow = state.Epoch, state.Sequence, state.Overflow
	if state.Dropped != nil {
		for key, seq := range state.Dropped {
			h.dropped[key] = seq
		}
	}
	for _, rec := range state.Records {
		h.records = append(h.records, rec)
	}
	if len(h.records) > h.capacity {
		evict := h.records[:len(h.records)-h.capacity]
		h.records = h.records[len(h.records)-h.capacity:]
		for _, rec := range evict {
			if rec.Event != nil && rec.Scope != "" && rec.Event.Sequence > h.dropped[rec.Scope] {
				h.dropped[rec.Scope] = rec.Event.Sequence
			}
		}
	}
	for _, saved := range state.Scopes {
		previous := make(map[string]watchEntry, len(saved.Previous))
		for path, entry := range saved.Previous {
			previous[path] = watchEntryRestored(entry)
		}
		h.scopes[saved.Key] = &watchScope{key: saved.Key, root: saved.Root, recursive: saved.Recursive, previous: previous, epoch: h.epoch, lastUse: time.Now()}
	}
	return nil
}

// parseWatchScopeKey recovers the structural identity encoded by
// watchScopeKey. It is intentionally independent of the persisted scope list:
// old records may outlive an evicted baseline, but their scope key still needs
// to be syntactically trustworthy before it can be used for replay.
func parseWatchScopeKey(key string) (root string, recursive bool, ok bool) {
	root, raw, found := strings.Cut(key, "\x00")
	if !found || root == "" || (raw != "true" && raw != "false") {
		return "", false, false
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", false, false
	}
	recursive, err := strconv.ParseBool(raw)
	if err != nil {
		return "", false, false
	}
	return root, recursive, true
}

func watchPathInScope(path, root string, recursive bool) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	// A baseline may include the watched root itself (for example, a provider
	// that snapshots directory metadata). It is always in scope; current poller
	// output usually omits it, but persisted validation must not reject this
	// legitimate representation.
	if path == root {
		return true
	}
	if root == "/" {
		// Every absolute non-root path is below `/`.
	} else if !strings.HasPrefix(path, root+"/") {
		return false
	}
	if !recursive {
		return filepath.Dir(path) == root
	}
	return true
}

func validWatchEntryType(t v1.EntryType) bool {
	switch t {
	case v1.EntryType_ENTRY_TYPE_FILE, v1.EntryType_ENTRY_TYPE_DIRECTORY, v1.EntryType_ENTRY_TYPE_SYMLINK:
		return true
	default:
		return false
	}
}

func validatePersistedWatchEntry(path string, entry persistedWatchEntry, root string, recursive bool) error {
	if path == "" || entry.Path == "" || path != entry.Path || !watchPathInScope(path, root, recursive) {
		return errors.New("watch journal contains an invalid scope snapshot path")
	}
	if !validWatchEntryType(entry.Type) || entry.Size < 0 {
		return errors.New("watch journal contains an invalid scope snapshot entry")
	}
	return nil
}

func validateWatchJournalEvent(event *v1.WatchEvent, root string, recursive bool) error {
	if event == nil || event.Sequence == 0 || event.Path == "" || !watchPathInScope(event.Path, root, recursive) || !validWatchEntryType(event.Type) || event.Size < 0 || event.Overflow || event.Reset_ || event.ResetReason != "" {
		return errors.New("watch journal contains an invalid event")
	}
	switch event.Operation {
	case "create", "modify", "chmod", "remove":
		if event.OldPath != "" {
			return errors.New("watch journal contains an invalid event old path")
		}
	case "rename":
		if event.OldPath == "" || event.OldPath == event.Path || !watchPathInScope(event.OldPath, root, recursive) {
			return errors.New("watch journal contains an invalid rename event")
		}
	default:
		return errors.New("watch journal contains an unknown event operation")
	}
	return nil
}

func (h *watchHub) persistedLocked() persistedWatchState {
	state := persistedWatchState{Epoch: h.epoch, Sequence: h.sequence, Overflow: h.overflow, Records: append([]watchEventRecord(nil), h.records...), Dropped: make(map[string]uint64, len(h.dropped)), Scopes: make([]persistedWatchScope, 0, len(h.scopes))}
	for key, seq := range h.dropped {
		state.Dropped[key] = seq
	}
	for key, scope := range h.scopes {
		entries := make(map[string]persistedWatchEntry, len(scope.previous))
		for path, entry := range scope.previous {
			entries[path] = watchEntryPersisted(entry)
		}
		state.Scopes = append(state.Scopes, persistedWatchScope{Key: key, Root: scope.root, Recursive: scope.recursive, Epoch: scope.epoch, Previous: entries})
	}
	sort.Slice(state.Scopes, func(i, j int) bool { return state.Scopes[i].Key < state.Scopes[j].Key })
	return state
}

// persistLocked writes a complete bounded snapshot through a temporary file,
// then renames it.  A torn write must never turn a valid cursor into malformed
// JSON after a transport restart.
func (h *watchHub) persistLocked() error {
	if h.path == "" {
		return nil
	}
	b, err := json.Marshal(h.persistedLocked())
	if err != nil {
		h.journalErr = err
		return watchJournalUnavailableError()
	}
	tmp := h.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		h.journalErr = err
		return watchJournalUnavailableError()
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		h.journalErr = err
		return watchJournalUnavailableError()
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		h.journalErr = err
		return watchJournalUnavailableError()
	}
	if err := f.Close(); err != nil {
		h.journalErr = err
		return watchJournalUnavailableError()
	}
	if err := os.Rename(tmp, h.path); err != nil {
		_ = os.Remove(tmp)
		h.journalErr = err
		return watchJournalUnavailableError()
	}
	return nil
}

func watchJournalUnavailableError() error {
	return connect.NewError(connect.CodeUnavailable, errors.New("filesystem watch journal unavailable"))
}

func watchStaleEpochError() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("stale placement epoch"))
}

func watchGapError() error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New("filesystem watch cursor gap; full rescan required"))
}

func watchOverflowMarker(epoch uint64) *v1.WatchEvent {
	return &v1.WatchEvent{Sequence: 0, PlacementEpoch: epoch, Overflow: true, Reset_: true, ResetReason: "overflow"}
}

// watchEventSender is the small stream seam needed when a sticky journal
// overflow is discovered before a subscriber exists. Sending the marker first
// keeps the recovery instruction on the wire even though the request then
// terminates with resource_exhausted.
type watchEventSender interface {
	Send(*v1.WatchEvent) error
}

func sendStickyWatchOverflow(stream watchEventSender, hub *watchHub, err error) error {
	if stream == nil || hub == nil || connect.CodeOf(err) != connect.CodeResourceExhausted {
		return err
	}
	hub.mu.Lock()
	overflow, epoch := hub.overflow, hub.epoch
	hub.mu.Unlock()
	if !overflow {
		return err
	}
	if sendErr := stream.Send(watchOverflowMarker(epoch)); sendErr != nil {
		return sendErr
	}
	return err
}

func watchScopeLimitError() error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New("filesystem watch scope limit reached"))
}

// setEpoch advances the placement epoch and invalidates all prior watch
// records.  Existing streams receive one reset marker before they close.
func (h *watchHub) setEpoch(epoch uint64) error {
	if epoch == 0 {
		epoch = 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadErr != nil || h.journalErr != nil {
		return watchJournalUnavailableError()
	}
	if h.epoch != 0 && epoch < h.epoch {
		return watchStaleEpochError()
	}
	if h.epoch == epoch {
		if h.overflow {
			return watchGapError()
		}
		return nil
	}
	for sub := range h.subs {
		h.closeSubscriberLocked(sub, &v1.WatchEvent{Sequence: 0, PlacementEpoch: epoch, Reset_: true, ResetReason: "placement"})
	}
	h.epoch = epoch
	h.sequence = 0
	h.overflow = false
	h.records = nil
	h.dropped = make(map[string]uint64)
	h.scopes = make(map[string]*watchScope)
	return h.persistLocked()
}

// ensureEpoch is used by every Watch request.  It is also intentionally
// exported within package main as ObserveWatchEpoch's implementation hook:
// callers that learn of a lifecycle epoch change can invalidate live streams
// even if no new Watch request has arrived yet.
func (h *watchHub) ensureEpoch(epoch uint64) error {
	if epoch == 0 {
		h.mu.Lock()
		epoch = h.epoch
		h.mu.Unlock()
		if epoch == 0 {
			epoch = 1
		}
	}
	return h.setEpoch(epoch)
}

func (h *watchHub) validateEpoch(epoch uint64) error {
	if err := h.journalFailure(); err != nil {
		return err
	}
	if epoch == 0 {
		return nil
	}
	h.mu.Lock()
	current := h.epoch
	h.mu.Unlock()
	if current != epoch {
		return watchStaleEpochError()
	}
	return nil
}

func (h *watchHub) scope(root string, recursive bool) (*watchScope, bool, error) {
	if err := h.journalFailure(); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	overflow := h.overflow
	h.mu.Unlock()
	if overflow {
		return nil, false, watchGapError()
	}
	root = filepath.Clean(root)
	if root == "." || root == "" {
		root = "/"
	}
	key := watchScopeKey(root, recursive)
	h.mu.Lock()
	h.pruneScopesLocked()
	if existing := h.scopes[key]; existing != nil && existing.epoch == h.epoch && existing.previous != nil {
		existing.lastUse = time.Now()
		h.mu.Unlock()
		return existing, false, nil
	}
	if len(h.scopes) >= watchMaxScopes {
		h.mu.Unlock()
		return nil, false, watchScopeLimitError()
	}
	epoch := h.epoch
	h.mu.Unlock()
	previous, err := collectWatch(root, recursive)
	if err != nil {
		return nil, false, fsErr("watch", root, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing := h.scopes[key]; existing != nil && existing.epoch == h.epoch && existing.previous != nil {
		existing.lastUse = time.Now()
		return existing, false, nil
	}
	h.pruneScopesLocked()
	if len(h.scopes) >= watchMaxScopes {
		return nil, false, watchScopeLimitError()
	}
	if epoch != h.epoch {
		// A concurrent epoch advance invalidated the snapshot we just collected.
		return nil, false, watchStaleEpochError()
	}
	scope := &watchScope{key: key, root: root, recursive: recursive, previous: previous, epoch: h.epoch, lastUse: time.Now()}
	h.scopes[key] = scope
	if err := h.persistLocked(); err != nil {
		delete(h.scopes, key)
		return nil, false, err
	}
	return scope, true, nil
}

func (h *watchHub) pruneScopesLocked() {
	if len(h.scopes) < watchMaxScopes {
		return
	}
	now := time.Now()
	for key, scope := range h.scopes {
		if scope.active == 0 && now.Sub(scope.lastUse) > watchScopeIdle {
			delete(h.scopes, key)
		}
	}
}

func watchEntryChanged(old, cur watchEntry) bool {
	// Directory mtimes change when children or runtime journals are updated.
	// Treating an mtime-only directory change as a guest event can make a
	// watcher rooted at `/` observe its own journal persistence forever.
	if old.type_ == v1.EntryType_ENTRY_TYPE_DIRECTORY && cur.type_ == v1.EntryType_ENTRY_TYPE_DIRECTORY {
		return old.mode != cur.mode || old.type_ != cur.type_ || old.ino != cur.ino || old.dev != cur.dev
	}
	return old.size != cur.size || old.mtime != cur.mtime || old.mode != cur.mode || old.type_ != cur.type_ || old.ino != cur.ino || old.dev != cur.dev
}

func watchEventFromEntry(e watchEntry, op, oldPath string, seq, epoch uint64) *v1.WatchEvent {
	return &v1.WatchEvent{Path: e.path, OldPath: oldPath, Operation: op, Type: e.type_, Size: e.size, MtimeUnixNano: e.mtime, Sequence: seq, PlacementEpoch: epoch}
}

type watchChange struct {
	entry   watchEntry
	oldPath string
	op      string
}

func watchIdentity(e watchEntry) string {
	if e.ino == 0 && e.dev == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", e.dev, e.ino)
}

// pollScope computes one deterministic snapshot delta and appends it to the
// placement journal.  The poll mutex prevents the background monitor and a
// reconnect's immediate catch-up from assigning duplicate changes.
func (h *watchHub) pollScope(scope *watchScope) error {
	if err := h.journalFailure(); err != nil {
		return err
	}
	scope.pollMu.Lock()
	defer scope.pollMu.Unlock()
	next, err := collectWatch(scope.root, scope.recursive)
	if err != nil {
		// A watched root can be removed while its stream is open. Treat that
		// observation as an empty snapshot so descendants produce explicit
		// remove events; a transient permission/I/O error remains a real source
		// failure and is retried by the monitor.
		if connect.CodeOf(err) == connect.CodeNotFound {
			next = make(map[string]watchEntry)
		} else {
			return err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overflow {
		return watchGapError()
	}
	if scope.epoch != h.epoch {
		return watchStaleEpochError()
	}
	oldSequence := h.sequence
	oldRecords := append([]watchEventRecord(nil), h.records...)
	oldDropped := make(map[string]uint64, len(h.dropped))
	for key, seq := range h.dropped {
		oldDropped[key] = seq
	}
	oldPrevious := scope.previous
	paths := make([]string, 0, len(scope.previous)+len(next))
	seen := make(map[string]struct{}, len(scope.previous)+len(next))
	for path := range scope.previous {
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for path := range next {
		if _, ok := seen[path]; !ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	changes := make([]watchChange, 0, len(paths))
	removed := make(map[string]watchEntry)
	created := make(map[string]watchEntry)
	for _, path := range paths {
		old, hadOld := scope.previous[path]
		cur, hadCur := next[path]
		if !hadOld && hadCur {
			created[path] = cur
			continue
		}
		if hadOld && !hadCur {
			removed[path] = old
			continue
		}
		if hadOld && hadCur && watchEntryChanged(old, cur) {
			op := "modify"
			if old.size == cur.size && old.mtime == cur.mtime && old.type_ == cur.type_ && old.ino == cur.ino && old.dev == cur.dev && old.mode != cur.mode {
				op = "chmod"
			}
			changes = append(changes, watchChange{entry: cur, op: op})
		}
	}
	// Pair create/remove entries by stable inode identity. This preserves a
	// rename's old path without claiming syscall-level completeness; when a
	// provider cannot expose identity the pair remains create+remove.
	removedByID := make(map[string][]string)
	for path, entry := range removed {
		if id := watchIdentity(entry); id != "" {
			removedByID[id] = append(removedByID[id], path)
		}
	}
	for id := range removedByID {
		sort.Strings(removedByID[id])
	}
	pairedRemoved := make(map[string]bool)
	pairedCreated := make(map[string]bool)
	for path, entry := range created {
		id := watchIdentity(entry)
		if id == "" || len(removedByID[id]) == 0 {
			continue
		}
		oldPath := removedByID[id][0]
		removedByID[id] = removedByID[id][1:]
		pairedRemoved[oldPath] = true
		pairedCreated[path] = true
		changes = append(changes, watchChange{entry: entry, oldPath: oldPath, op: "rename"})
	}
	for path, entry := range created {
		if !pairedCreated[path] {
			changes = append(changes, watchChange{entry: entry, op: "create"})
		}
	}
	for path, entry := range removed {
		if !pairedRemoved[path] {
			changes = append(changes, watchChange{entry: entry, op: "remove"})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].entry.path != changes[j].entry.path {
			return changes[i].entry.path < changes[j].entry.path
		}
		return changes[i].op < changes[j].op
	})
	newEvents := make([]*v1.WatchEvent, 0, len(changes))
	for _, change := range changes {
		if h.sequence == ^uint64(0) {
			h.overflow = true
			// Keep the exhausted high-water marker at MaxUint64 even though
			// this poll's partially assigned changes are rolled back. The marker
			// must survive reload and distinguish a permanent cursor gap from a
			// torn journal whose last retained record simply lags the watermark.
			h.sequence = ^uint64(0)
			h.records = oldRecords
			h.dropped = oldDropped
			scope.previous = oldPrevious
			for sub := range h.subs {
				h.closeSubscriberLocked(sub, watchOverflowMarker(h.epoch))
			}
			_ = h.persistLocked()
			return watchGapError()
		}
		h.sequence++
		event := watchEventFromEntry(change.entry, change.op, change.oldPath, h.sequence, h.epoch)
		h.records = append(h.records, watchEventRecord{Scope: scope.key, Event: event})
		for len(h.records) > h.capacity {
			oldest := h.records[0]
			h.records = h.records[1:]
			if oldest.Event != nil && oldest.Scope != "" && oldest.Event.Sequence > h.dropped[oldest.Scope] {
				h.dropped[oldest.Scope] = oldest.Event.Sequence
			}
		}
		newEvents = append(newEvents, event)
	}
	scope.previous = next
	scope.lastUse = time.Now()
	if len(newEvents) == 0 {
		return nil
	}
	// Commit the cursor and snapshot before making an event visible. If the
	// journal write fails, no subscriber is allowed to advance a cursor for a
	// record that a transport restart could lose; the next poll retries the
	// atomic journal write.
	if err := h.persistLocked(); err != nil {
		h.sequence = oldSequence
		h.records = oldRecords
		h.dropped = oldDropped
		scope.previous = oldPrevious
		return err
	}
	for _, event := range newEvents {
		h.publishLocked(scope.key, event)
	}
	return nil
}

func (h *watchHub) publishLocked(scope string, event *v1.WatchEvent) {
	for sub := range h.subs {
		if sub.scope != scope {
			continue
		}
		select {
		case sub.ch <- watchNotice{Event: cloneWatchEvent(event)}:
		default:
			h.closeSubscriberLocked(sub, watchOverflowMarker(h.epoch))
		}
	}
}

// subscribe atomically checks the scope watermark, snapshots replay, and joins
// the live fan-out. Replay is returned separately so the live queue remains a
// small, fixed bound even when the retained journal is large.
func (h *watchHub) subscribe(scope *watchScope, after uint64, initial bool) (*watchSubscriber, []*v1.WatchEvent, error) {
	if err := h.journalFailure(); err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overflow {
		return nil, nil, watchGapError()
	}
	if scope.epoch != h.epoch {
		return nil, nil, watchStaleEpochError()
	}
	sub := &watchSubscriber{scope: scope.key, ch: make(chan watchNotice, watchSubscriberWindow)}
	replay := make([]*v1.WatchEvent, 0)
	if initial && after == 0 {
		replay = append(replay, &v1.WatchEvent{Sequence: 0, PlacementEpoch: h.epoch, Reset_: true, ResetReason: "initial"})
	}
	// A cursor below the retained watermark is an explicit gap.  Treating
	// sequence zero as a special success here would make an old subscriber
	// silently skip the evicted prefix after a transport restart.
	if after > h.sequence {
		sub.mu.Lock()
		sub.terminal = &watchNotice{Event: watchOverflowMarker(h.epoch)}
		sub.terminalErr = watchGapError()
		sub.mu.Unlock()
		replay = append(replay, cloneWatchEvent(sub.terminal.Event))
		close(sub.ch)
		return sub, replay, nil
	}
	if (initial && after > 0) || (after > 0 && after < h.dropped[scope.key]) {
		sub.mu.Lock()
		sub.terminal = &watchNotice{Event: watchOverflowMarker(h.epoch)}
		sub.terminalErr = watchGapError()
		replay = append(replay, cloneWatchEvent(sub.terminal.Event))
		sub.mu.Unlock()
		close(sub.ch)
		return sub, replay, nil
	} else {
		for _, rec := range h.records {
			if rec.Scope == scope.key && rec.Event != nil && rec.Event.Sequence > after {
				replay = append(replay, cloneWatchEvent(rec.Event))
			}
		}
	}
	h.subs[sub] = struct{}{}
	scope.active++
	scope.lastUse = time.Now()
	return sub, replay, nil
}

func cloneWatchEvent(in *v1.WatchEvent) *v1.WatchEvent {
	if in == nil {
		return nil
	}
	return proto.Clone(in).(*v1.WatchEvent)
}

func (h *watchHub) closeSubscriberLocked(sub *watchSubscriber, terminal *v1.WatchEvent) {
	h.closeSubscriberLockedWithError(sub, terminal, nil)
}

func (h *watchHub) closeSubscriberLockedWithError(sub *watchSubscriber, terminal *v1.WatchEvent, terminalErr error) {
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	sub.mu.Lock()
	if sub.terminal == nil && terminal != nil {
		sub.terminal = &watchNotice{Event: terminal}
		if terminal.Overflow {
			sub.terminalErr = watchGapError()
		}
	}
	if terminalErr != nil {
		sub.terminalErr = terminalErr
	}
	// Keep only the terminal marker.  The channel is closed while holding the
	// hub lock, so publish cannot race this drain/close sequence.
	for len(sub.ch) > 0 {
		<-sub.ch
	}
	if sub.terminal != nil && sub.terminal.Event != nil {
		// The marker is queued before close so a consumer that only reads the
		// stream channel cannot miss the recovery instruction.  The fallback in
		// Watch's closed-channel branch covers an unusual send failure.
		sub.ch <- *sub.terminal
	}
	sub.mu.Unlock()
	if scope := h.scopes[sub.scope]; scope != nil && scope.active > 0 {
		scope.active--
		scope.lastUse = time.Now()
	}
	close(sub.ch)
}

func (h *watchHub) unsubscribe(sub *watchSubscriber) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	h.closeSubscriberLocked(sub, nil)
	h.mu.Unlock()
}

func (sub *watchSubscriber) terminalEvent() *v1.WatchEvent {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.terminal == nil {
		return nil
	}
	return cloneWatchEvent(sub.terminal.Event)
}

func (sub *watchSubscriber) terminalError() error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	return sub.terminalErr
}

func (h *watchHub) startMonitor(scope *watchScope) {
	h.mu.Lock()
	if scope.running {
		h.mu.Unlock()
		return
	}
	scope.running = true
	scope.lastUse = time.Now()
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			if scope.epoch == h.epoch {
				scope.running = false
			}
			h.mu.Unlock()
		}()
		ticker := time.NewTicker(watchPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := h.pollScope(scope); err != nil {
				// Epoch changes intentionally terminate the old monitor.  Other
				// source errors are terminal for this scope. Repeatedly hiding a
				// permission or I/O failure would leave a caller waiting forever
				// with no way to distinguish a quiet tree from a dead source.
				if connect.CodeOf(err) == connect.CodeFailedPrecondition {
					return
				}
				h.mu.Lock()
				for sub := range h.subs {
					if sub.scope != scope.key {
						continue
					}
					h.closeSubscriberLockedWithError(sub, nil, err)
				}
				h.mu.Unlock()
				return
			}
			h.mu.Lock()
			if scope.epoch != h.epoch || (scope.active == 0 && time.Since(scope.lastUse) > watchScopeIdle) {
				h.mu.Unlock()
				return
			}
			h.mu.Unlock()
		}
	}()
}

// ObserveEpoch is the lifecycle hook used by an outer proxy or supervisor
// that learns about a new placement while a stream is idle.  Existing streams
// receive the same reset marker as a new Watch request.
func (h *watchHub) ObserveEpoch(epoch uint64) error {
	return h.ensureEpoch(epoch)
}

// Watch is the Connect-facing placement-scoped stream.
func (s *fsServer) Watch(ctx context.Context, req *connect.Request[v1.WatchRequest], stream *connect.ServerStream[v1.WatchEvent]) error {
	if req == nil || req.Msg == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing watch request"))
	}
	m := req.Msg
	root := m.Path
	if root == "" {
		root = "/"
	}
	if !filepath.IsAbs(root) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("watch path must be absolute"))
	}
	s.watchMu.Lock()
	if s.watch == nil {
		s.watch = newWatchHub(s.watchPath, watchBufferEvents)
	}
	hub := s.watch
	s.watchMu.Unlock()
	if err := hub.ensureEpoch(m.PlacementEpoch); err != nil {
		return sendStickyWatchOverflow(stream, hub, err)
	}
	if err := hub.validateEpoch(m.PlacementEpoch); err != nil {
		return sendStickyWatchOverflow(stream, hub, err)
	}
	scope, initial, err := hub.scope(root, m.Recursive)
	if err != nil {
		return sendStickyWatchOverflow(stream, hub, err)
	}
	if !initial {
		// Catch up changes made while no stream (or transport child) was
		// present before registering the new subscriber.
		if err := hub.pollScope(scope); err != nil {
			return sendStickyWatchOverflow(stream, hub, err)
		}
	}
	sub, replay, err := hub.subscribe(scope, m.AfterSequence, initial)
	if err != nil {
		return sendStickyWatchOverflow(stream, hub, err)
	}
	for _, event := range replay {
		if err := stream.Send(event); err != nil {
			hub.unsubscribe(sub)
			return err
		}
	}
	if len(replay) == 0 {
		// Flush the server-stream headers before waiting for the first change.
		// Connect's streaming client otherwise cannot return from Watch until a
		// payload arrives, preventing the caller from performing the write that
		// would produce that payload.
		if err := stream.Send(nil); err != nil {
			hub.unsubscribe(sub)
			return err
		}
	}
	if err := sub.terminalError(); err != nil {
		return err
	}
	hub.startMonitor(scope)
	defer hub.unsubscribe(sub)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case notice, ok := <-sub.ch:
			if !ok {
				return sub.terminalError()
			}
			if notice.Event == nil {
				continue
			}
			if err := stream.Send(notice.Event); err != nil {
				return err
			}
		}
	}
}
