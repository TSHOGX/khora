package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"connectrpc.com/connect"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// --- fs service (the transport data plane fs primitive, fs transport) --------------------------
//
// Unary metadata/content calls plus a bounded Watch stream. Paths are GUEST-ABSOLUTE and NOT confined to a root: the caller is the
// engine (trusted, the external API assumes an authorized caller) and the guest runs as root by design
// (the guest identity model), so the isolation boundary is the sandbox itself — sentry, not a path check. A
// confinement would break the contract where callers legitimately touch /etc, /root and /usr.
//
// Confinement WOULD be required if a path component ever came from data rather
// than from the caller (an archive entry name, a template import). That is not
// this surface.

type fsServer struct {
	// activity is the SAME sandbox-global clock the process service and Health share
	// (the activity signal). Present because fs traffic IS I/O by that section's criterion —
	// "every operation the engine mediates for the guest, not only process frames".
	//
	// Without it a sandbox being actively written to looks idle for as long as nobody types,
	// and the auto-pause that follows discards the very work in flight. Stamped in the
	// handlers rather than by each caller, like proc.broadcast's stamp.
	activity  *activityClock
	watch     *watchHub
	watchMu   sync.Mutex
	watchPath string
}

type watchEntry struct {
	path  string
	type_ v1.EntryType
	size  int64
	mtime int64
	mode  uint32
	ino   uint64
	dev   uint64
}

func collectWatch(root string, recursive bool) (map[string]watchEntry, error) {
	out := make(map[string]watchEntry)
	// These files are transport-owned journals/pid portals. They are not guest
	// workload content; observing the journal itself would create a feedback
	// loop because every persisted watch event changes its mtime. Match exact
	// paths so a guest file merely sharing the prefix remains observable.
	internalPaths := map[string]struct{}{
		"/tmp/.sandboxd-watch.json":     {},
		"/tmp/.sandboxd-processes.json": {},
		"/tmp/.sandboxd-transport.pid":  {},
		"/tmp/.sandboxd-crashloop":      {},
	}
	isInternal := func(p string) bool {
		_, ok := internalPaths[filepath.Clean(p)]
		return ok
	}
	walk := func(p string, fi os.FileInfo) {
		if isInternal(p) {
			return
		}
		kind := v1.EntryType_ENTRY_TYPE_FILE
		if fi.Mode()&os.ModeSymlink != 0 {
			kind = v1.EntryType_ENTRY_TYPE_SYMLINK
		} else if fi.IsDir() {
			kind = v1.EntryType_ENTRY_TYPE_DIRECTORY
		}
		var ino, dev uint64
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			ino, dev = uint64(stat.Ino), uint64(stat.Dev)
		}
		out[p] = watchEntry{path: p, type_: kind, size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: uint32(fi.Mode()), ino: ino, dev: dev}
	}
	if !recursive {
		ents, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, ent := range ents {
			p := filepath.Join(root, ent.Name())
			if isInternal(p) {
				continue
			}
			fi, err := os.Lstat(p)
			if err == nil {
				walk(p, fi)
			}
		}
		return out, nil
	}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if isInternal(p) {
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p != root {
			walk(p, info)
		}
		return nil
	})
	return out, err
}

// stampIO records fs traffic on the shared activity clock (the activity signal). Called by
// the DATA operations — Read / Write / MakeDir / Remove / Move — and deliberately NOT by
// Stat / List.
//
// That exclusion is the load-bearing half. Stat and List are the calls an external watcher
// makes in a loop, so stamping them would let an observer keep a sandbox permanently "active"
// — the defect transport behavior and the transactional outbox both refuse (an observation must not be indistinguishable from
// the thing observed) — and would silently delete the idle disposition rather than fail.
//
// Read DOES stamp: it moves file contents, which is a guest being used, not a poll of its
// metadata. A caller wanting to keep a sandbox alive has the explicit RecordActivity RPC.
func (s *fsServer) stampIO() {
	if s != nil {
		s.activity.stamp()
	}
}

// fsErr is the ONE errno → Connect code mapping for this service. Centralized on
// purpose: a per-handler mapping drifts, and the drift is invisible because each
// handler looks locally reasonable. Every fs handler returns through here.
func fsErr(op, path string, err error) error {
	if err == nil {
		return nil
	}
	code := connect.CodeInternal
	// ORDER IS LOAD-BEARING: the specific errnos come first, because Go's generic
	// sentinels are broader than they look. errors.Is(ENOTEMPTY, fs.ErrExist) is
	// TRUE, so testing fs.ErrExist earlier would report "directory not empty" as
	// already_exists and send the caller down the wrong recovery path.
	switch {
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EISDIR):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, syscall.EXDEV):
		// Reachable here in ordinary use, unlike on a single-filesystem host: the
		// guest namespace stitches together the overlay upper, tmpfs /tmp and
		// Volume mounts, so a Move between two normal-looking paths can
		// cross devices. Reported explicitly rather than falling through to
		// `internal`, which would read as an engine bug instead of "rename cannot
		// do this; copy then delete".
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New(op+" "+path+": cross-device move: rename(2) cannot move between filesystems — copy then remove instead"))
	case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENAMETOOLONG), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
		code = connect.CodeInvalidArgument
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		// The resource admission capacity cap surfacing as an ordinary write failure: the
		// sandbox stays alive and only its own write fails (the data-plane exposure boundary).
		code = connect.CodeResourceExhausted
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOENT):
		code = connect.CodeNotFound
	case errors.Is(err, fs.ErrExist), errors.Is(err, syscall.EEXIST):
		code = connect.CodeAlreadyExists
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		code = connect.CodePermissionDenied
	}
	return connect.NewError(code, errors.New(op+" "+path+": "+err.Error()))
}

// entryInfo is the ONE EntryInfo constructor, shared by Stat and List so the two
// can never report the same path differently. fi must come from an LSTAT (a
// symlink is described as itself), and path must be absolute.
func entryInfo(path string, fi os.FileInfo) *v1.EntryInfo {
	e := &v1.EntryInfo{
		Name:          fi.Name(),
		Path:          path,
		Type:          v1.EntryType_ENTRY_TYPE_FILE,
		Size:          fi.Size(),
		Mode:          uint32(fi.Mode()),
		MtimeUnixNano: fi.ModTime().UnixNano(),
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		e.Type = v1.EntryType_ENTRY_TYPE_SYMLINK
		// Absolutize against the LINK's own directory: a relative target is
		// meaningless to a caller outside the guest, and there is no Readlink RPC
		// to resolve it with afterwards.
		if tgt, err := os.Readlink(path); err == nil {
			if !filepath.IsAbs(tgt) {
				tgt = filepath.Join(filepath.Dir(path), tgt)
			}
			e.SymlinkTarget = &tgt
		}
	case fi.IsDir():
		e.Type = v1.EntryType_ENTRY_TYPE_DIRECTORY
	}
	return e
}

func (s *fsServer) Read(_ context.Context, req *connect.Request[v1.ReadRequest]) (*connect.Response[v1.ReadResponse], error) {
	// Stamped on ARRIVAL, before the (possibly slow) syscall and regardless of whether
	// it succeeds — the request arriving IS the activity, exactly as writeInput stamps
	// before its blocking Write. A stamp only on success would let a run of EACCES look
	// like an idle sandbox.
	s.stampIO()
	m := req.Msg
	f, err := os.Open(m.Path)
	if err != nil {
		// Through the shared mapper: a permission failure must not report as
		// not-found just because it happened on an open.
		return nil, fsErr("read", m.Path, err)
	}
	defer f.Close()
	if m.Offset > 0 {
		if _, err := f.Seek(m.Offset, io.SeekStart); err != nil {
			return nil, fsErr("read", m.Path, err)
		}
	}
	var data []byte
	if m.Length > 0 {
		// `length` is an UPPER BOUND, never an allocation size. It arrives off the
		// wire with no relation to the file, so `make([]byte, m.Length)` let one
		// number size a guest-side allocation: measured on a 10-byte file, 1<<34
		// allocated 16 GiB, and 1<<50 was an outright `makeslice: len out of range`
		// panic. The sandbox has a RAM cap and is TERMINATED on exceeding it
		// (resource admission), so that is a remote kill switch, which is why transport behavior calls "neither
		// end holds the whole thing" a SECURITY property and not an efficiency one.
		// LimitReader + a buffer that grows with ARRIVING bytes costs what the file
		// actually yields.
		//
		// Deliberately NOT clamped by the file's reported Size(): measured,
		// /proc/self/status reports size 0 while yielding ~1.1 KB, so a size
		// clamp reads a whole class of files as EMPTY — a silent wrong answer,
		// strictly worse than the allocation it was meant to bound.
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(io.LimitReader(f, m.Length)); err != nil {
			return nil, fsErr("read", m.Path, err)
		}
		data = buf.Bytes()
	} else {
		b, err := io.ReadAll(f)
		if err != nil {
			return nil, fsErr("read", m.Path, err)
		}
		data = b
	}
	return connect.NewResponse(&v1.ReadResponse{Data: data}), nil
}

func (s *fsServer) Write(_ context.Context, req *connect.Request[v1.WriteRequest]) (*connect.Response[v1.WriteResponse], error) {
	s.stampIO() // arrival, not success (see Read)
	m := req.Msg
	if err := os.WriteFile(m.Path, m.Data, 0o644); err != nil {
		return nil, fsErr("write", m.Path, err)
	}
	return connect.NewResponse(&v1.WriteResponse{BytesWritten: int64(len(m.Data))}), nil
}

func (*fsServer) Stat(_ context.Context, req *connect.Request[v1.StatRequest]) (*connect.Response[v1.StatResponse], error) {
	p := req.Msg.Path
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fsErr("stat", p, err)
	}
	return connect.NewResponse(&v1.StatResponse{Entry: entryInfo(p, fi)}), nil
}

func (*fsServer) List(_ context.Context, req *connect.Request[v1.FsServiceListRequest]) (*connect.Response[v1.FsServiceListResponse], error) {
	m := req.Msg
	// depth 0 is INVALID, not "unlimited": an accidental zero value would
	// otherwise silently walk an entire filesystem.
	if m.Depth < 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("list "+m.Path+": depth must be >= 1"))
	}
	entries, err := listDir(m.Path, int(m.Depth))
	if err != nil {
		return nil, fsErr("list", m.Path, err)
	}
	return connect.NewResponse(&v1.FsServiceListResponse{Entries: entries}), nil
}

// listDir walks up to depth levels below root and returns a FLAT list. Flat
// rather than nested because every entry carries its absolute path, which makes
// the parent unambiguous without a tree shape on the wire.
//
// A directory that cannot be read mid-walk is SKIPPED rather than failing the
// whole call: a listing is an observation, and one unreadable subdirectory should
// not deny the caller everything else. An unreadable ROOT still errors (below).
func listDir(root string, depth int) ([]*v1.EntryInfo, error) {
	top, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]*v1.EntryInfo, 0, len(top))
	type level struct {
		dir  string
		ents []os.DirEntry
		d    int
	}
	queue := []level{{dir: root, ents: top, d: 1}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, de := range cur.ents {
			p := filepath.Join(cur.dir, de.Name())
			// Lstat, not de.Info(): consistent symlink reporting with Stat.
			fi, err := os.Lstat(p)
			if err != nil {
				continue // vanished mid-walk; not the caller's problem
			}
			out = append(out, entryInfo(p, fi))
			if fi.IsDir() && cur.d < depth {
				sub, err := os.ReadDir(p)
				if err != nil {
					continue // unreadable subtree: skip, keep the rest
				}
				queue = append(queue, level{dir: p, ents: sub, d: cur.d + 1})
			}
		}
	}
	return out, nil
}

func (s *fsServer) MakeDir(_ context.Context, req *connect.Request[v1.MakeDirRequest]) (*connect.Response[v1.MakeDirResponse], error) {
	s.stampIO() // arrival, not success (see Read)
	p := req.Msg.Path
	// Report an existing path rather than succeeding silently — the caller may
	// treat already_exists as benign, but the server does not erase the fact.
	// Checked before MkdirAll because MkdirAll is itself idempotent.
	if fi, err := os.Lstat(p); err == nil {
		if fi.IsDir() {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				errors.New("mkdir "+p+": directory already exists"))
		}
		return nil, connect.NewError(connect.CodeAlreadyExists,
			errors.New("mkdir "+p+": path exists and is not a directory"))
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return nil, fsErr("mkdir", p, err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fsErr("mkdir", p, err)
	}
	return connect.NewResponse(&v1.MakeDirResponse{Entry: entryInfo(p, fi)}), nil
}

func (s *fsServer) Remove(_ context.Context, req *connect.Request[v1.RemoveRequest]) (*connect.Response[v1.RemoveResponse], error) {
	s.stampIO() // arrival, not success (see Read)
	p := req.Msg.Path
	// Refuse "/" outright. The caller is trusted but not infallible, and a
	// recursive delete of the guest root is not a request anyone means.
	if filepath.Clean(p) == "/" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("remove /: refusing to delete the guest root"))
	}
	// RemoveAll: recursive AND idempotent on a missing path (it returns nil), so
	// "delete this" converges instead of erroring on a second call.
	if err := os.RemoveAll(p); err != nil {
		return nil, fsErr("remove", p, err)
	}
	return connect.NewResponse(&v1.RemoveResponse{}), nil
}

func (s *fsServer) Move(_ context.Context, req *connect.Request[v1.MoveRequest]) (*connect.Response[v1.MoveResponse], error) {
	s.stampIO() // arrival, not success (see Read)
	m := req.Msg
	if err := os.MkdirAll(filepath.Dir(m.Destination), 0o755); err != nil {
		return nil, fsErr("move", m.Destination, err)
	}
	// rename(2): atomic, and it OVERWRITES an existing destination file. Stated
	// in the RPC contract rather than guarded here — a caller that needs
	// fail-if-exists can Stat first, and silently refusing would break the
	// common "replace this file" case.
	if err := os.Rename(m.Source, m.Destination); err != nil {
		return nil, fsErr("move "+m.Source+" →", m.Destination, err)
	}
	fi, err := os.Lstat(m.Destination)
	if err != nil {
		return nil, fsErr("move", m.Destination, err)
	}
	return connect.NewResponse(&v1.MoveResponse{Entry: entryInfo(m.Destination, fi)}), nil
}
