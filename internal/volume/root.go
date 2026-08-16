package volume

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// localRoot implements Root over a host directory using ONE syscall for the
// entire security boundary: openat2(dirfd, path, RESOLVE_IN_ROOT).
//
// WHY THIS AND NOT chroot / A MOUNT NAMESPACE. RESOLVE_IN_ROOT gives path semantics
// IDENTICAL to chroot's, with no privilege, no mount
// namespace, and no thread pinning. Four properties measured on this substrate's
// 6.x kernel, read and write side both, and the code below depends on all four:
// ".." at the root CLAMPS (it resolves to the root itself, it is not an error) · an
// ABSOLUTE path is REINTERPRETED against that root · a symlink pointing outside the
// root (relative or absolute) resolves BACK INSIDE it · a symlink loop reports ELOOP.
//
// The cost comparison is why we do not copy a reference implementation's shape: this is
// ONE syscall, against "one fresh mount namespace + bind + pivot_root PER REQUEST" for
// the same guarantee — orders of magnitude apart by construction, and that shape also
// needs root and an OS-thread ownership protocol. Copy the capability, not the mechanism.
//
// FAILURE OF THIS BOUNDARY IS SILENT: a write through an escaping path SUCCEEDS
// and lands outside. That is why root confinement has its own unit test (a pure
// unit test — it needs a directory, not runsc, which is part of what makes it
// cheap) rather than being assumed from the flag being present.
type localRoot struct {
	// dirfd is an O_PATH descriptor for the volume root. Every operation resolves
	// against it, so the volume root cannot be swapped underneath us by a rename of
	// some ancestor after the handle was opened.
	dirfd  int
	volume string
	closed bool
}

// NewLocalRoot opens a root-confined accessor for a host directory.
//
// EXPORTED BUT NOT A SEAM MEMBER: an intra-subsystem helper, so BOTH arms in this
// package share ONE accessor implementation rather than two copies that can drift.
// Nothing above the Volume subsystem calls it — above the seam a volume is an ID and Backend.OpenRoot
// is the only way to reach its bytes (the ID-addressed Overlay model / the API error contract). A remote arm would implement
// Root over its own transport; the interface is paths and bytes, not host primitives.
func NewLocalRoot(volumeID, dir string) (Root, error) {
	// O_PATH: we only ever use this fd as a resolution anchor, never to read or
	// write through directly, so we ask for no access at all.
	fd, err := unix.Openat2(unix.AT_FDCWD, dir, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("volume: open root for %s: %w", volumeID, err)
	}
	return &localRoot{dirfd: fd, volume: volumeID}, nil
}

func (r *localRoot) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return unix.Close(r.dirfd)
}

// resolve opens one path INSIDE the root. This is the whole confinement boundary:
// every other method funnels through it, so there is exactly one place where the
// RESOLVE_IN_ROOT flag has to be right.
func (r *localRoot) resolve(name string, flags int, perm fs.FileMode) (int, error) {
	if r.closed {
		return -1, fs.ErrClosed
	}
	rel := cleanRel(name)
	fd, err := unix.Openat2(r.dirfd, rel, &unix.OpenHow{
		Flags: uint64(flags | unix.O_CLOEXEC),
		Mode:  uint64(perm.Perm()),
		// RESOLVE_IN_ROOT is the confinement. RESOLVE_NO_MAGICLINKS additionally
		// refuses /proc-style magic links, which are the one link kind that could
		// otherwise jump outside a root that happens to contain a procfs mount.
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return -1, &fs.PathError{Op: "openat2", Path: name, Err: err}
	}
	return fd, nil
}

// cleanRel normalizes a caller path into something openat2 accepts relative to the
// root fd.
//
// It does NOT provide the security boundary — RESOLVE_IN_ROOT does. Its job is
// only to turn the shapes a caller may legitimately send ("/data", "data/",
// "./data") into one spelling, and to avoid handing openat2 an empty string. An
// absolute path is left absolute deliberately: RESOLVE_IN_ROOT reinterprets it
// against the root, which is exactly the chroot semantic we promise, so stripping
// the slash here would be redundant rather than protective.
func cleanRel(name string) string {
	c := path.Clean("/" + strings.TrimSpace(name))
	if c == "/" {
		return "."
	}
	return strings.TrimPrefix(c, "/")
}

// splitParent splits a path into the parent directory to resolve and the single
// final component to operate on. Needed by the operations whose syscalls take a
// dirfd + basename (mkdirat / unlinkat / renameat) and therefore cannot express
// RESOLVE_IN_ROOT for the final component themselves — resolving the PARENT under
// RESOLVE_IN_ROOT and then acting on a plain basename keeps the boundary intact,
// because the basename can no longer contain a traversal.
func splitParent(name string) (parent, base string, err error) {
	rel := cleanRel(name)
	if rel == "." {
		return "", "", &fs.PathError{Op: "split", Path: name, Err: errors.New("refers to the volume root")}
	}
	parent, base = path.Split(rel)
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		parent = "."
	}
	// Cleaning above already removed "." / ".." components, so this cannot be a
	// traversal; the check states the invariant rather than enforcing it, and would
	// catch a future edit to cleanRel that weakened it.
	if base == "" || base == "." || base == ".." {
		return "", "", &fs.PathError{Op: "split", Path: name, Err: errors.New("not a single path component")}
	}
	return parent, base, nil
}

// parentFD resolves the parent directory of name inside the root.
func (r *localRoot) parentFD(name string) (int, string, error) {
	parent, base, err := splitParent(name)
	if err != nil {
		return -1, "", err
	}
	fd, err := r.resolve(parent, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, "", err
	}
	return fd, base, nil
}

func (r *localRoot) Stat(name string) (FileInfo, error) {
	// No O_NOFOLLOW: a symlink SHOULD be followed, because RESOLVE_IN_ROOT keeps
	// the result inside the volume either way — that is the measured property this
	// accessor rests on.
	fd, err := r.resolve(name, unix.O_PATH, 0)
	if err != nil {
		return FileInfo{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return FileInfo{}, &fs.PathError{Op: "fstat", Path: name, Err: err}
	}
	return infoFromStat(path.Base(cleanRel(name)), &st), nil
}

func infoFromStat(name string, st *unix.Stat_t) FileInfo {
	mode := fs.FileMode(st.Mode & 0o7777)
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if isDir {
		mode |= fs.ModeDir
	}
	return FileInfo{
		Name:    name,
		Size:    st.Size,
		Mode:    mode,
		IsDir:   isDir,
		ModTime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
	}
}

func (r *localRoot) List(name string) ([]FileInfo, error) {
	fd, err := r.resolve(name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	// os.NewFile takes ownership of fd, so f.Close covers it. The fd stays valid for
	// the fstatat calls below while f is open.
	f := os.NewFile(uintptr(fd), cleanRel(name))
	defer f.Close()
	ents, err := f.Readdirnames(-1)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]FileInfo, 0, len(ents))
	for _, base := range ents {
		// Metadata comes from fstatat AGAINST THIS DIRECTORY FD, never from
		// os.DirEntry.Info(). That method re-stats using the *os.File's NAME as a path,
		// which for a descriptor obtained by openat2 is a path relative to the PROCESS
		// CWD and therefore resolves somewhere else entirely — measured: every entry
		// came back ENOENT and a "skip what vanished" branch silently dropped the whole
		// listing while reporting success. Going through the fd also keeps the
		// confinement property: nothing here re-resolves a caller-visible path.
		var st unix.Stat_t
		if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if err == unix.ENOENT {
				continue // genuinely vanished mid-listing: skip, do not fail the whole read
			}
			return nil, &fs.PathError{Op: "fstatat", Path: path.Join(name, base), Err: err}
		}
		out = append(out, infoFromStat(base, &st))
	}
	return out, nil
}

func (r *localRoot) ReadAt(name string, p []byte, off int64) (int, error) {
	fd, err := r.resolve(name, unix.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	n, err := unix.Pread(fd, p, off)
	if err != nil {
		return 0, &fs.PathError{Op: "pread", Path: name, Err: err}
	}
	if n < len(p) {
		// io.Reader convention: a short read at end of file reports io.EOF, so a
		// streaming export can terminate on it rather than needing the file size.
		return n, io.EOF
	}
	return n, nil
}

func (r *localRoot) WriteAt(name string, p []byte, off int64) (int, error) {
	fd, err := r.resolve(name, unix.O_WRONLY|unix.O_CREAT, 0o644)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	n, err := unix.Pwrite(fd, p, off)
	if err != nil {
		// Deliberately NOT wrapped away: a capacity cap surfaces here as ENOSPC and
		// the caller must be able to see it (resource admission: past the cap a write gets a plain
		// ENOSPC, and that is an expected data-plane result, not an engine failure).
		return n, &fs.PathError{Op: "pwrite", Path: name, Err: err}
	}
	return n, nil
}

func (r *localRoot) Mkdir(name string, perm fs.FileMode) error {
	pfd, base, err := r.parentFD(name)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	if err := unix.Mkdirat(pfd, base, uint32(perm.Perm())); err != nil {
		return &fs.PathError{Op: "mkdirat", Path: name, Err: err}
	}
	return nil
}

func (r *localRoot) Remove(name string) error {
	pfd, base, err := r.parentFD(name)
	if err != nil {
		// An absent PARENT means the target is absent too, which the contract defines as
		// a no-op — not an error. Reporting the parent's ENOENT here would break
		// idempotency in a way that only shows up on the paths that legitimately clear a
		// not-yet-existing file (import does exactly that before writing), so the
		// distinction is made here rather than left to each caller.
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(pfd)
	err = unix.Unlinkat(pfd, base, 0)
	if err == unix.EISDIR || err == unix.EPERM {
		// Directories need AT_REMOVEDIR. Linux reports EISDIR, some filesystems EPERM.
		err = unix.Unlinkat(pfd, base, unix.AT_REMOVEDIR)
	}
	if err != nil {
		if err == unix.ENOENT {
			return nil // idempotent, like every other reclamation path in the engine
		}
		return &fs.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}

func (r *localRoot) Rename(oldName, newName string) error {
	ofd, obase, err := r.parentFD(oldName)
	if err != nil {
		return err
	}
	defer unix.Close(ofd)
	nfd, nbase, err := r.parentFD(newName)
	if err != nil {
		return err
	}
	defer unix.Close(nfd)
	if err := unix.Renameat(ofd, obase, nfd, nbase); err != nil {
		return &fs.PathError{Op: "renameat", Path: oldName + " -> " + newName, Err: err}
	}
	return nil
}
