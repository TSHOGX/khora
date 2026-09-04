package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

func watchNoticeEvent(t *testing.T, sub *watchSubscriber) *v1.WatchEvent {
	t.Helper()
	select {
	case notice := <-sub.ch:
		if notice.Event == nil {
			t.Fatal("watch notice has no event")
		}
		return notice.Event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watch notice")
		return nil
	}
}

func watchReplayEvent(t *testing.T, replay []*v1.WatchEvent, index int) *v1.WatchEvent {
	t.Helper()
	if index < 0 || index >= len(replay) {
		t.Fatalf("replay index %d out of %d", index, len(replay))
	}
	return replay[index]
}

func writeWatchFile(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatchHubCursorReplayAndOrdering(t *testing.T) {
	root := t.TempDir()
	h := newWatchHub("", 8)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	scope, initial, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !initial {
		t.Fatal("first scope was not marked initial")
	}
	sub, replay, err := h.subscribe(scope, 0, initial)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(sub)
	if len(replay) != 1 || !watchReplayEvent(t, replay, 0).Reset_ || replay[0].ResetReason != "initial" || replay[0].Sequence != 0 {
		t.Fatalf("initial notice = %+v", replay)
	}

	writeWatchFile(t, root, "a")
	if err := h.pollScope(scope); err != nil {
		t.Fatal(err)
	}
	one := watchNoticeEvent(t, sub)
	if one.Sequence != 1 || one.Operation != "create" {
		t.Fatalf("first event = %+v", one)
	}
	writeWatchFile(t, root, "b")
	if err := h.pollScope(scope); err != nil {
		t.Fatal(err)
	}
	two := watchNoticeEvent(t, sub)
	if two.Sequence != 2 || two.Operation != "create" {
		t.Fatalf("second event = %+v", two)
	}
	h.unsubscribe(sub)

	// A reconnect asks after sequence 1 and receives only the retained tail;
	// sequence assignment is hub-owned, not reset by the new stream.
	replaySub, replay, err := h.subscribe(scope, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(replaySub)
	got := watchReplayEvent(t, replay, 0)
	if got.Sequence != 2 || got.Path != two.Path {
		t.Fatalf("replayed event = %+v; want sequence 2", got)
	}
	select {
	case extra := <-replaySub.ch:
		t.Fatalf("replay returned duplicate/old event: %+v", extra.Event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestWatchHubBoundedCursorGapAndZeroStartsAtRetainedTail(t *testing.T) {
	root := t.TempDir()
	h := newWatchHub("", 2)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	scope, _, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		writeWatchFile(t, root, name)
		if err := h.pollScope(scope); err != nil {
			t.Fatal(err)
		}
	}
	if h.dropped[scope.key] != 2 {
		t.Fatalf("dropped watermark = %d; want 2", h.dropped[scope.key])
	}

	// Zero means "start at the earliest retained event" and remains useful
	// after trimming; it is not itself an expired cursor.
	tail, replay, err := h.subscribe(scope, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(tail)
	if got := watchReplayEvent(t, replay, 0); got.Sequence != 3 {
		t.Fatalf("zero cursor first retained sequence = %d; want 3", got.Sequence)
	}
	if got := watchReplayEvent(t, replay, 1); got.Sequence != 4 {
		t.Fatalf("zero cursor second retained sequence = %d; want 4", got.Sequence)
	}

	gap, replay, err := h.subscribe(scope, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(gap)
	got := watchReplayEvent(t, replay, 0)
	if !got.Overflow || !got.Reset_ || got.ResetReason != "overflow" {
		t.Fatalf("expired cursor notice = %+v", got)
	}
}

func TestWatchHubEpochChangeClosesWithReset(t *testing.T) {
	root := t.TempDir()
	h := newWatchHub("", 4)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	scope, initial, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	sub, _, err := h.subscribe(scope, 0, initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setEpoch(2); err != nil {
		t.Fatal(err)
	}
	if got := watchNoticeEvent(t, sub); !got.Reset_ || got.ResetReason != "placement" || got.PlacementEpoch != 2 || got.Sequence != 0 {
		t.Fatalf("placement reset = %+v", got)
	}
	select {
	case _, ok := <-sub.ch:
		if ok {
			t.Fatal("epoch reset subscriber remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("epoch reset subscriber did not close")
	}

	newScope, newInitial, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !newInitial || newScope.epoch != 2 {
		t.Fatalf("new scope after epoch = initial:%v epoch:%d", newInitial, newScope.epoch)
	}
}

func TestWatchHubPersistsCursorAndSnapshotAcrossTransportRestart(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "watch.json")
	h := newWatchHub(state, 8)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	scope, _, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	writeWatchFile(t, root, "persisted")
	if err := h.pollScope(scope); err != nil {
		t.Fatal(err)
	}

	restarted := newWatchHub(state, 8)
	if err := restarted.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	restored, initial, err := restarted.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if initial {
		t.Fatal("persisted scope was treated as a new baseline")
	}
	sub, replay, err := restarted.subscribe(restored, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.unsubscribe(sub)
	got := watchReplayEvent(t, replay, 0)
	if got.Sequence != 1 || got.Path != filepath.Join(root, "persisted") {
		t.Fatalf("persisted replay = %+v", got)
	}
}

func TestWatchHubCorruptJournalFailsClosed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "watch.json")
	if err := os.WriteFile(state, []byte(`{"epoch":1,"sequence":`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newWatchHub(state, 8)
	if err := h.ensureEpoch(1); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("corrupt journal ensure epoch = %v; want unavailable", err)
	}
	if err := h.ObserveEpoch(2); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("corrupt journal observe epoch = %v; want unavailable", err)
	}
	if _, _, err := h.scope(t.TempDir(), true); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("corrupt journal scope = %v; want unavailable", err)
	}
}

func TestWatchHubUnreadableJournalFailsClosed(t *testing.T) {
	// A directory at the journal path produces a deterministic read error on all
	// supported hosts without relying on process privileges or global filesystem
	// permissions.
	state := filepath.Join(t.TempDir(), "watch.json")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	h := newWatchHub(state, 8)
	if err := h.ensureEpoch(1); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("unreadable journal ensure epoch = %v; want unavailable", err)
	}
	if err := h.journalFailure(); err == nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("journal failure was not retained as unavailable: %v", err)
	}
}

func TestWatchHubRejectsNonCanonicalPersistedScope(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	cases := []struct {
		name   string
		mutate func(*persistedWatchScope)
	}{
		{
			name: "relative root",
			mutate: func(scope *persistedWatchScope) {
				scope.Root = "relative/root"
				scope.Key = watchScopeKey(scope.Root, scope.Recursive)
			},
		},
		{
			name: "unclean root",
			mutate: func(scope *persistedWatchScope) {
				scope.Root = root + "/."
				scope.Key = watchScopeKey(scope.Root, scope.Recursive)
			},
		},
		{
			name: "key mismatch",
			mutate: func(scope *persistedWatchScope) {
				scope.Key = "tampered"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "watch.json")
			scope := persistedWatchScope{Root: root, Recursive: true, Epoch: 1, Previous: map[string]persistedWatchEntry{}}
			scope.Key = watchScopeKey(scope.Root, scope.Recursive)
			tc.mutate(&scope)
			raw, err := json.Marshal(persistedWatchState{
				Epoch: 1, Sequence: 0, Records: []watchEventRecord{}, Scopes: []persistedWatchScope{scope},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			h := newWatchHub(statePath, 8)
			if err := h.ensureEpoch(1); connect.CodeOf(err) != connect.CodeUnavailable {
				t.Fatalf("tampered scope journal ensure epoch = %v; want unavailable", err)
			}
			if _, _, err := h.scope(root, true); connect.CodeOf(err) != connect.CodeUnavailable {
				t.Fatalf("tampered scope journal scope = %v; want unavailable", err)
			}
		})
	}
}

func TestWatchHubRejectsMalformedPersistedScopeAndEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	validScope := persistedWatchScope{
		Key:       watchScopeKey(root, true),
		Root:      root,
		Recursive: true,
		Epoch:     1,
		Previous: map[string]persistedWatchEntry{
			filepath.Join(root, "file"): {Path: filepath.Join(root, "file"), Type: v1.EntryType_ENTRY_TYPE_FILE},
		},
	}
	validEvent := &v1.WatchEvent{
		Path: filepath.Join(root, "file"), Operation: "create", Type: v1.EntryType_ENTRY_TYPE_FILE,
		Sequence: 1, PlacementEpoch: 1,
	}
	cases := []struct {
		name  string
		state persistedWatchState
	}{
		{
			name: "nil previous snapshot",
			state: persistedWatchState{Epoch: 1, Scopes: []persistedWatchScope{func() persistedWatchScope {
				s := validScope
				s.Previous = nil
				return s
			}()}},
		},
		{
			name: "snapshot outside scope",
			state: persistedWatchState{Epoch: 1, Scopes: []persistedWatchScope{func() persistedWatchScope {
				s := validScope
				s.Previous = map[string]persistedWatchEntry{
					"/outside": {Path: "/outside", Type: v1.EntryType_ENTRY_TYPE_FILE},
				}
				return s
			}()}},
		},
		{
			name: "snapshot has unknown type",
			state: persistedWatchState{Epoch: 1, Scopes: []persistedWatchScope{func() persistedWatchScope {
				s := validScope
				s.Previous = map[string]persistedWatchEntry{
					filepath.Join(root, "file"): {Path: filepath.Join(root, "file"), Type: v1.EntryType_ENTRY_TYPE_UNSPECIFIED},
				}
				return s
			}()}},
		},
		{
			name: "event has unknown operation",
			state: persistedWatchState{Epoch: 1, Sequence: 1, Records: []watchEventRecord{{Scope: validScope.Key, Event: func() *v1.WatchEvent {
				e := proto.Clone(validEvent).(*v1.WatchEvent)
				e.Operation = "unknown"
				return e
			}()}}, Scopes: []persistedWatchScope{validScope}},
		},
		{
			name: "event path outside scope",
			state: persistedWatchState{Epoch: 1, Sequence: 1, Records: []watchEventRecord{{Scope: validScope.Key, Event: func() *v1.WatchEvent {
				e := proto.Clone(validEvent).(*v1.WatchEvent)
				e.Path = "/outside"
				return e
			}()}}, Scopes: []persistedWatchScope{validScope}},
		},
		{
			name: "rename missing old path",
			state: persistedWatchState{Epoch: 1, Sequence: 1, Records: []watchEventRecord{{Scope: validScope.Key, Event: func() *v1.WatchEvent {
				e := proto.Clone(validEvent).(*v1.WatchEvent)
				e.Operation = "rename"
				return e
			}()}}, Scopes: []persistedWatchScope{validScope}},
		},
		{
			name: "event carries reset marker",
			state: persistedWatchState{Epoch: 1, Sequence: 1, Records: []watchEventRecord{{Scope: validScope.Key, Event: func() *v1.WatchEvent {
				e := proto.Clone(validEvent).(*v1.WatchEvent)
				e.Reset_ = true
				return e
			}()}}, Scopes: []persistedWatchScope{validScope}},
		},
		{
			name:  "dropped watermark has invalid scope key",
			state: persistedWatchState{Epoch: 1, Sequence: 1, Dropped: map[string]uint64{"not-a-scope": 1}, Scopes: []persistedWatchScope{validScope}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "watch.json")
			raw, err := json.Marshal(tc.state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			h := newWatchHub(statePath, 8)
			if err := h.ensureEpoch(1); connect.CodeOf(err) != connect.CodeUnavailable {
				t.Fatalf("malformed journal ensure epoch = %v; want unavailable", err)
			}
		})
	}
}

func TestWatchHubRejectsPersistedScopeOverflow(t *testing.T) {
	base := t.TempDir()
	scopes := make([]persistedWatchScope, 0, watchMaxScopes+1)
	for i := 0; i < watchMaxScopes+1; i++ {
		root := filepath.Join(base, "root", fmt.Sprintf("%d", i))
		scopes = append(scopes, persistedWatchScope{
			Key: watchScopeKey(root, true), Root: root, Recursive: true, Epoch: 1,
			Previous: map[string]persistedWatchEntry{},
		})
	}
	statePath := filepath.Join(t.TempDir(), "watch.json")
	raw, err := json.Marshal(persistedWatchState{Epoch: 1, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newWatchHub(statePath, 8)
	if err := h.ensureEpoch(1); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("overflow journal ensure epoch = %v; want unavailable", err)
	}
}

func TestWatchHubUnknownInitialCursorRequiresRescan(t *testing.T) {
	root := t.TempDir()
	h := newWatchHub("", 4)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	scope, initial, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	sub, replay, err := h.subscribe(scope, 99, initial)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(sub)
	got := watchReplayEvent(t, replay, 0)
	if !got.Overflow || !got.Reset_ || got.ResetReason != "overflow" || got.PlacementEpoch != 1 || got.Sequence != 0 {
		t.Fatalf("unknown initial cursor = %+v", got)
	}
}

func TestWatchHubSlowSubscriberGetsBoundedOverflowReset(t *testing.T) {
	h := newWatchHub("", 512)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	scope, initial, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	sub, replay, err := h.subscribe(scope, 0, initial)
	if err != nil {
		t.Fatal(err)
	}
	// Leave the subscriber undrained after consuming only the initial marker.
	if len(replay) != 1 || !replay[0].Reset_ {
		t.Fatalf("initial marker = %+v", replay)
	}

	h.mu.Lock()
	for i := 1; i <= cap(sub.ch)+1; i++ {
		h.sequence++
		event := &v1.WatchEvent{Path: filepath.Join(root, "event"), Operation: "modify", Sequence: h.sequence, PlacementEpoch: h.epoch}
		h.publishLocked(scope.key, event)
	}
	h.mu.Unlock()

	// The queue is bounded: the overflow marker replaces the stale queued
	// records and closes the subscriber. A caller can therefore rescan without
	// waiting for an unbounded producer/consumer backlog to drain.
	got := watchNoticeEvent(t, sub)
	if !got.Overflow || !got.Reset_ || got.ResetReason != "overflow" || got.Sequence != 0 {
		t.Fatalf("subscriber overflow marker = %+v", got)
	}
	select {
	case _, ok := <-sub.ch:
		if ok {
			t.Fatal("overflow subscriber remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("overflow subscriber did not close")
	}
	if sub.terminalError() == nil {
		t.Fatal("overflow subscriber did not retain cursor-gap error")
	}
}

func TestWatchHubDetectsRenameAndModeChange(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	if err := os.WriteFile(oldPath, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWatchHub("", 8)
	if err := h.ensureEpoch(1); err != nil {
		t.Fatal(err)
	}
	scope, _, err := h.scope(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if err := h.pollScope(scope); err != nil {
		t.Fatal(err)
	}
	if len(h.records) != 1 || h.records[0].Event.Operation != "rename" || h.records[0].Event.OldPath != oldPath || h.records[0].Event.Path != newPath {
		t.Fatalf("rename records = %+v", h.records)
	}
	if err := os.Chmod(newPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.pollScope(scope); err != nil {
		t.Fatal(err)
	}
	if len(h.records) != 2 || h.records[1].Event.Operation != "chmod" {
		t.Fatalf("mode-change records = %+v", h.records)
	}
}

func TestWatchHubIgnoresDirectoryMtimeOnlyChanges(t *testing.T) {
	old := watchEntry{path: "/root", type_: v1.EntryType_ENTRY_TYPE_DIRECTORY, mtime: 1, mode: 0o755, ino: 2, dev: 3}
	cur := old
	cur.mtime = 2
	if watchEntryChanged(old, cur) {
		t.Fatal("directory mtime-only change should not be reported as a guest change")
	}
}
