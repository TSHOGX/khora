package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCLIDocIsNotStale(t *testing.T) {
	want := renderCLIDoc(newRootCommand())
	path := filepath.Join("..", "..", cliDocPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate: ./scripts/generate-docs.sh)", path, err)
	}
	if !bytes.Equal(got, want) {
		at := 0
		for at < len(got) && at < len(want) && got[at] == want[at] {
			at++
		}
		endGot, endWant := min(at+80, len(got)), min(at+80, len(want))
		t.Errorf("%s is stale at byte %d; regenerate with ./scripts/generate-docs.sh\nchecked in: %q\ngenerated:  %q", cliDocPath, at, got[at:endGot], want[at:endWant])
	}
}

func TestCLIDocCoversEveryVisibleCommand(t *testing.T) {
	root := newRootCommand()
	doc := string(renderCLIDoc(root))
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			if !cmd.IsAvailableCommand() || cmd.Hidden {
				continue
			}
			heading := "## `" + cmd.CommandPath() + "`"
			if !strings.Contains(doc, heading) {
				t.Errorf("generated documentation is missing %s", cmd.CommandPath())
			}
			walk(cmd)
		}
	}
	walk(root)
}
