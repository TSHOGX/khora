package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/pkg/khora"
)

func TestInfoPreservesIdentityAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	var first khora.Info
	for i := 0; i < 2; i++ {
		st, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		eng := &Engine{Store: st}
		srv := httptest.NewServer(eng.HTTPHandler())
		client, err := khora.New(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		info, err := client.Info(context.Background())
		srv.Close()
		if closeErr := st.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.StoreID == "" || info.Version != "dev" {
			t.Fatalf("info: %+v", info)
		}
		if i == 0 {
			first = info
		} else if info != first {
			t.Fatalf("restart changed info: %+v -> %+v", first, info)
		}
	}
}

type unreadableIdentity struct{ store.Store }

func (unreadableIdentity) Identity(context.Context) (model.StoreID, error) {
	return "", fmt.Errorf("read identity /srv/khora/private/store.db: unavailable")
}

func TestInfoFailureIsUncachedAndSanitized(t *testing.T) {
	eng := &Engine{Store: unreadableIdentity{store.NewMemory()}}
	rr := httptest.NewRecorder()
	eng.HTTPHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/v1/info", nil))
	if rr.Code != http.StatusInternalServerError || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response: %d %v", rr.Code, rr.Header())
	}
	if body := rr.Body.String(); strings.Contains(body, "/srv/") || !strings.Contains(body, "[redacted]") {
		t.Fatalf("unsanitized response: %s", body)
	}
}

func TestInfoMatchesStoreAndBuild(t *testing.T) {
	st := store.NewMemory()
	id, err := st.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eng := &Engine{Store: st, version: "test-build"}
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()
	client, err := khora.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"test-build", "another-build"} {
		eng.version = version
		info, err := client.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.StoreID != string(id) || info.Version != version {
			t.Fatalf("info: %+v", info)
		}
	}
	rr := httptest.NewRecorder()
	eng.handleInfo(rr, httptest.NewRequest("GET", "/v1/info", nil))
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable identity")
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body) != 2 {
		t.Fatalf("body: %s", rr.Body.String())
	}
}
