//go:build substrate

// L2 acceptance for the §5.E fs surface (transport D4.4) against the REAL
// sandboxd under real runsc: Stat / List / MakeDir / Remove / Move on a live
// guest filesystem. Requires root.
//
//	sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 ./test/ -run TestFs
package test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/TSHOGX/warren/internal/transport"
	"github.com/TSHOGX/warren/test/harness"
)

// The five metadata RPCs against a real guest. Unit tests cover the errno table
// and the depth walk on a host tmpdir; what only the substrate can prove is that
// each call actually crosses the wire and lands on the guest's own filesystem
// (gofer-proxied overlay upper, not the host's).
func TestFsSurfaceRoundTrip(t *testing.T) {
	// The bare Template ships no coreutils, so the symlink fixture comes from an
	// injected static probe rather than `ln` (which is not present).
	cl, done := launchWith(t, func(templatesDir, template string) {
		harness.InjectStaticBin(t, templatesDir, template, "symlinker", symlinkSrc)
	})
	defer done()
	ctx := context.Background()

	// --- MakeDir creates parents (mkdir -p), in one call ---------------------
	nested := "/root/a/b/c"
	e, err := cl.MakeDir(ctx, nested)
	if err != nil {
		t.Fatalf("MakeDir %s: %v", nested, err)
	}
	if e.Type != transport.EntryDirectory {
		t.Errorf("MakeDir returned type %q; want dir", e.Type)
	}
	if e.Path != nested {
		t.Errorf("MakeDir returned path %q; want %q", e.Path, nested)
	}
	// The intermediate levels really exist — that is what "creates parents" means.
	if _, err := cl.Stat(ctx, "/root/a/b"); err != nil {
		t.Fatalf("Stat intermediate /root/a/b: %v — MakeDir did not create parents", err)
	}
	t.Logf("✓ MakeDir created the full chain %s in one call", nested)

	// --- MakeDir on an existing dir is already_exists, not silent success ----
	if _, err := cl.MakeDir(ctx, nested); err == nil {
		t.Error("MakeDir on an existing directory succeeded; want already_exists (the server must not erase the distinction)")
	} else if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Errorf("MakeDir on existing dir: code=%v; want already_exists", got)
	}

	// --- Stat on a regular file ---------------------------------------------
	body := []byte("hello fs surface")
	f := filepath.Join(nested, "file.txt")
	if err := cl.Write(ctx, f, body); err != nil {
		t.Fatalf("Write %s: %v", f, err)
	}
	st, err := cl.Stat(ctx, f)
	if err != nil {
		t.Fatalf("Stat %s: %v", f, err)
	}
	if st.Type != transport.EntryFile {
		t.Errorf("Stat type = %q; want file", st.Type)
	}
	if st.Size != int64(len(body)) {
		t.Errorf("Stat size = %d; want %d", st.Size, len(body))
	}
	if st.Name != "file.txt" {
		t.Errorf("Stat name = %q; want file.txt", st.Name)
	}
	if st.MTime.IsZero() {
		t.Error("Stat mtime is zero — the guest's mtime did not cross the wire")
	}
	t.Logf("✓ Stat: type=%s size=%d mode=%#o mtime=%s", st.Type, st.Size, st.Mode&0o777, st.MTime.Format("15:04:05"))

	// --- Stat on a missing path is not_found, not internal -------------------
	if _, err := cl.Stat(ctx, "/root/definitely-not-here"); err == nil {
		t.Error("Stat on a missing path succeeded")
	} else if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Errorf("Stat missing path: code=%v; want not_found", got)
	}

	// --- Stat follows LSTAT semantics on a symlink --------------------------
	// Built in-guest via ln -s so the link is the guest's own, with a RELATIVE
	// target — the absolutization is the part a host caller depends on.
	if out, code := runGuestBin(t, cl, "/usr/bin/symlinker", "file.txt", filepath.Join(nested, "link.txt")); code != 0 {
		t.Fatalf("create symlink in guest: exit=%d out=%s", code, out)
	}
	link := filepath.Join(nested, "link.txt")
	ls, err := cl.Stat(ctx, link)
	if err != nil {
		t.Fatalf("Stat symlink: %v", err)
	}
	if ls.Type != transport.EntrySymlink {
		t.Errorf("Stat on a symlink reported %q; want symlink (lstat, not stat — a followed link would report 'file')", ls.Type)
	}
	if ls.SymlinkTarget != f {
		t.Errorf("symlink target = %q; want the absolutized %q (a relative target is meaningless to a host caller)", ls.SymlinkTarget, f)
	}
	t.Logf("✓ Stat is lstat: symlink reported as itself, target absolutized to %s", ls.SymlinkTarget)

	// --- List: depth 1 vs depth 3, flattened -------------------------------
	d1, err := cl.ListDir(ctx, "/root/a", 1)
	if err != nil {
		t.Fatalf("ListDir depth 1: %v", err)
	}
	if len(d1) != 1 || d1[0].Name != "b" {
		t.Errorf("ListDir /root/a depth 1 = %+v; want exactly [b]", names(d1))
	}
	d3, err := cl.ListDir(ctx, "/root/a", 3)
	if err != nil {
		t.Fatalf("ListDir depth 3: %v", err)
	}
	// Flattened: b, b/c, b/c/file.txt, b/c/link.txt all appear at the top level of
	// the result, each carrying its own absolute path.
	want := map[string]bool{"/root/a/b": false, "/root/a/b/c": false, f: false, link: false}
	for _, e := range d3 {
		if _, ok := want[e.Path]; ok {
			want[e.Path] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("ListDir depth 3 missing %s; got %v", p, names(d3))
		}
	}
	t.Logf("✓ List flattened depth 3 → %d entries, each with an absolute path", len(d3))

	// --- List rejects depth 0 rather than walking everything -----------------
	if _, err := cl.ListDir(ctx, "/root", 0); err == nil {
		t.Error("ListDir depth 0 succeeded; want invalid_argument (0 must not mean 'unlimited')")
	} else if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("ListDir depth 0: code=%v; want invalid_argument", got)
	}

	// --- Move: creates destination parents, overwrites, returns the new entry -
	dst := "/root/moved/deeper/renamed.txt"
	me, err := cl.Move(ctx, f, dst)
	if err != nil {
		t.Fatalf("Move %s → %s: %v", f, dst, err)
	}
	if me.Path != dst {
		t.Errorf("Move returned path %q; want the DESTINATION %q", me.Path, dst)
	}
	if _, err := cl.Stat(ctx, f); err == nil {
		t.Error("source still exists after Move — that is a copy, not a move")
	}
	got, err := cl.Read(ctx, dst, 0, 0)
	if err != nil || string(got) != string(body) {
		t.Errorf("Read after Move = %q, %v; want the original bytes", got, err)
	}
	t.Logf("✓ Move created destination parents and carried the bytes")

	// Move ONTO an existing file overwrites (rename(2) semantics, stated in the
	// contract). Pinned because the alternative — silent refusal — would break
	// "replace this file" for every caller.
	victim := "/root/moved/victim.txt"
	if err := cl.Write(ctx, victim, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Move(ctx, dst, victim); err != nil {
		t.Fatalf("Move onto an existing destination: %v; want overwrite", err)
	}
	if got, _ := cl.Read(ctx, victim, 0, 0); string(got) != string(body) {
		t.Errorf("after overwrite Read = %q; want the moved bytes", got)
	}

	// --- Remove: recursive, and idempotent on a missing path ----------------
	if err := cl.Remove(ctx, "/root/a"); err != nil {
		t.Fatalf("Remove /root/a (recursive): %v", err)
	}
	if _, err := cl.Stat(ctx, "/root/a"); err == nil {
		t.Error("/root/a still exists after Remove")
	}
	// Second Remove must SUCCEED — "delete this" converges rather than erroring on
	// a retry. Both reference implementations agree on this.
	if err := cl.Remove(ctx, "/root/a"); err != nil {
		t.Errorf("Remove on an already-absent path returned %v; want success (idempotent)", err)
	}
	t.Logf("✓ Remove is recursive and idempotent")

	// --- Remove refuses "/" ------------------------------------------------
	if err := cl.Remove(ctx, "/"); err == nil {
		t.Error("Remove / succeeded — the guest root must be refused")
	}
}

// names renders entry paths for a failure message.
func names(entries []transport.EntryInfo) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// symlinkSrc is a static symlink(2) probe. The fs RPCs deliberately expose no
// "create symlink" verb (nothing needs it), and the bare Template has no `ln`, so
// the lstat fixture is built with this.
//
//	Args: symlinker <target> <linkpath>
const symlinkSrc = `package main

import (
	"fmt"
	"os"
)

func main() {
	if err := os.Symlink(os.Args[1], os.Args[2]); err != nil {
		fmt.Printf("SYMLINK_ERR=%v\n", err)
		os.Exit(1)
	}
	fmt.Println("LINKED")
}
`

// runGuestBin runs a guest binary and returns its combined output and exit code.
// Used to build fixtures the fs RPCs deliberately cannot.
func runGuestBin(t *testing.T, cl transport.Client, cmd string, args ...string) (string, int) {
	t.Helper()
	ch, err := cl.Start(context.Background(), transport.StartSpec{Cmd: cmd, Args: args})
	if err != nil {
		t.Fatalf("guest exec %s %v: %v", cmd, args, err)
	}
	var sb strings.Builder
	code := -1
	for ev := range ch {
		switch ev.Kind {
		case "data":
			sb.Write(ev.Data)
		case "end":
			code = ev.ExitCode
		}
	}
	return sb.String(), code
}
