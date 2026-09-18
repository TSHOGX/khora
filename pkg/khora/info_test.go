package khora

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInfoReadsEveryCall(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/info" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, `{"store_id":"store_%d","version":"dev"}`, calls)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	first, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || first.StoreID == second.StoreID {
		t.Fatal("identity was cached")
	}
}
