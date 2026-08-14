package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBaseComparisonIncludesCommittedAndWorkingTreeChanges(t *testing.T) {
	dir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	runGit(t, "init", "-q")
	runGit(t, "config", "user.name", "Khora Test")
	runGit(t, "config", "user.email", "test@example.invalid")
	writeTestFile(t, "engine.go", "package engine\n\nconst Version = 1\n")
	runGit(t, "add", "engine.go")
	runGit(t, "commit", "-qm", "base")
	runGit(t, "branch", "-M", "main")
	runGit(t, "switch", "-qc", "feature")

	writeTestFile(t, "engine.go", "package engine\n\nconst Version = 2\n")
	runGit(t, "add", "engine.go")
	runGit(t, "commit", "-qm", "feature")
	writeTestFile(t, "notes.md", "uncommitted\n")

	comparison, err := comparisonRef("main")
	if err != nil {
		t.Fatalf("comparisonRef: %v", err)
	}
	changes, err := collect(comparison)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	got := make(map[string]string)
	for _, change := range changes {
		got[change.path] = change.status
	}
	if got["engine.go"] != "M" {
		t.Errorf("committed feature change status = %q, want M; all changes: %v", got["engine.go"], got)
	}
	if got["notes.md"] != "A" {
		t.Errorf("untracked working-tree change status = %q, want A; all changes: %v", got["notes.md"], got)
	}
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
