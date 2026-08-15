// Package allocbytes measures how many bytes a directory tree actually OCCUPIES,
// from the host's own allocation statement.
//
// It is a LEAF package and deliberately ignorant of the engine's model — it takes
// a path and returns a byte count. the Rootfs subsystem (uncapped Overlay/layer bytes) and the Volume subsystem
// (uncapped Volume bytes) both need the SAME measurement for the second term of
// the resource admission node-capacity inequality, and "subsystems never call each other" (the lifecycle)
// makes a shared leaf the sanctioned answer rather than an import edge or two
// drifting copies (the documentation: internal/guestport is the precedent).
//
// WHY ALLOCATED BLOCKS AND NOT APPARENT SIZE. A sparse file's length is not what
// it costs: a 512 MiB hole-punched file occupies zero blocks, and charging its
// length would refuse creates the node could comfortably hold (resource admission forbids that
// direction explicitly). Conversely the number must come from the HOST, never from
// a guest: the sentry fabricates st_blocks, so a guest-side read of the same file
// reports blocks derived from its length while the host's true allocation is 0.
package allocbytes

import (
	"os"
	"path/filepath"
	"syscall"
)

// Tree reports the bytes allocated to the tree rooted at root, counting regular
// files only.
//
// Hardlinked files are counted ONCE (dedup by dev+ino): a shipped Template tree
// can carry them, and the same physical bytes must not be charged twice (the
// resource admission "at most once for physically shared bytes" invariant). A file that
// disappears mid-walk (a concurrent destroy) is skipped rather than failing the
// caller — this runs against live storage by design. A missing root is zero, not
// an error, for the same reason.
func Tree(root string) (int64, error) {
	var total int64
	type inode struct {
		dev uint64
		ino uint64
	}
	seen := map[inode]bool{}
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			// No stat available (should not happen on Linux): fall back to apparent
			// size rather than silently counting zero, which would undercount.
			total += info.Size()
			return nil
		}
		if st.Nlink > 1 {
			k := inode{dev: uint64(st.Dev), ino: st.Ino}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		total += st.Blocks * 512
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return total, nil
}
