package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	transportv1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect"
)

func TestProcessClientUsesConnectMountAndSandboxHeader(t *testing.T) {
	oldAPI, oldClient := apiURL, httpClient
	t.Cleanup(func() { apiURL, httpClient = oldAPI, oldClient })

	var gotPath, gotSandbox string
	mux := http.NewServeMux()
	mux.Handle(transportv1connect.NewProcessServiceHandler(&listProcessServer{
		inspect: func(r *connect.Request[transportv1.ListRequest]) {
			gotSandbox = r.Header().Get(processSandboxIDHeader)
		},
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	apiURL, httpClient = server.URL+"/v1", server.Client()

	_, err := processClient().List(context.Background(), processRequest("sb-1", &transportv1.ListRequest{}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := transportv1connect.ProcessServiceListProcedure; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	if gotSandbox != "sb-1" {
		t.Errorf("%s = %q, want %q", processSandboxIDHeader, gotSandbox, "sb-1")
	}
}

type listProcessServer struct {
	transportv1connect.UnimplementedProcessServiceHandler
	inspect func(*connect.Request[transportv1.ListRequest])
}

func (s *listProcessServer) List(_ context.Context, req *connect.Request[transportv1.ListRequest]) (*connect.Response[transportv1.ListResponse], error) {
	s.inspect(req)
	return connect.NewResponse(&transportv1.ListResponse{}), nil
}
