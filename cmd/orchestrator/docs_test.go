package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrchestratorDocIsNotStale(t *testing.T) {
	want := renderOrchestratorDoc()
	path := filepath.Join("..", "..", orchestratorDocPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate: ./scripts/generate-docs.sh)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale; regenerate with ./scripts/generate-docs.sh", orchestratorDocPath)
	}
}

func TestOrchestratorFlagHelpIsOneLine(t *testing.T) {
	var values orchestratorFlags
	fs := newOrchestratorFlagSet("orchestrator", flag.ContinueOnError, &values)
	fs.VisitAll(func(f *flag.Flag) {
		if strings.ContainsAny(f.Usage, "\r\n") {
			t.Errorf("-%s help is not one line", f.Name)
		}
	})
}
