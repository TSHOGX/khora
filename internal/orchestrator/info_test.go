package orchestrator

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/pkg/khora"
)

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
