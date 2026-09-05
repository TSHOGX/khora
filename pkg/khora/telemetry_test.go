package khora

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestTelemetryClientPreservesMaximumUint64Cursor(t *testing.T) {
	const sandboxID = "sbx_cursor"
	maximum := ^uint64(0)
	wantAfter := strconv.FormatUint(maximum, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/"+sandboxID+"/telemetry" {
			t.Errorf("request path = %q; want telemetry route", r.URL.Path)
		}
		if got := r.URL.Query().Get("after"); got != wantAfter {
			t.Errorf("after query = %q; want %q", got, wantAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"samples":[],"earliest":3,"latest":3}`)
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.Telemetry(context.Background(), sandboxID, maximum, 0)
	if err != nil {
		t.Fatalf("Telemetry(max uint64): %v", err)
	}
	if len(page.Samples) != 0 || page.Earliest != 3 || page.Latest != 3 {
		t.Fatalf("page = %+v; want empty page with bounds 3/3", page)
	}
}
