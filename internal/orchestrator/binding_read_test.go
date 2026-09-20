package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/TSHOGX/khora/pkg/khora"
)

func TestBindingReadReportsConditionalVolumeDetachWithoutPrivatePaths(t *testing.T) {
	srv, _ := bindingFixture(t)
	volume := newVolume(t, srv)
	response := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", `{"template_id":"bare","cpu_millis":500,"memory_byte":134217728,"disk_byte":134217728,"idle_timeout":"0s","retention":"0s","network":{"egress":{"default":"deny","allow":[{"cidr":"192.0.2.0/24","port":443,"proto":"tcp"}]}},"volumes":[{"volume_id":"`+volume+`","path":"/data","access":"exclusive"}]}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", response.StatusCode)
	}
	var created createResp
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	client, err := sdk.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before, err := client.Binding(ctx, created.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if before.MutationRevision == "" || before.TemplateID != "bare" || before.CPUMillis != 500 || before.MemoryByte != 134217728 || before.DiskByte != 134217728 || before.IdleTimeout != "0s" || before.Retention != "0s" || before.State != "running" || before.PlacementState != "active" || len(before.Volumes) != 1 || before.Volumes[0].VolumeID != volume || before.Volumes[0].Access != "exclusive" || before.Volumes[0].Path != "/data" {
		t.Fatalf("binding read differs from accepted declaration: %+v", before)
	}
	if len(before.Network.Egress.Allow) != 1 || before.Network.Egress.Allow[0].CIDR != "192.0.2.0/24" {
		t.Fatalf("binding lost network declaration: %+v", before.Network)
	}
	get, err := client.Get(ctx, created.SandboxID)
	if err != nil || get.MutationRevision != before.MutationRevision {
		t.Fatalf("binding validator differs from control read: %+v %v", get, err)
	}
	if _, err := client.PauseIfMatch(ctx, created.SandboxID, before.MutationRevision); err != nil {
		t.Fatal(err)
	}
	paused, err := client.Binding(ctx, created.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	empty := []sdk.VolumeBinding{}
	if _, err := client.PatchIfMatch(ctx, created.SandboxID, paused.MutationRevision, sdk.PatchRequest{Volumes: &empty}); err != nil {
		t.Fatal(err)
	}
	detached, err := client.Binding(ctx, created.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if detached.Volumes == nil || len(detached.Volumes) != 0 || detached.MutationRevision == paused.MutationRevision || detached.PlacementEpoch != paused.PlacementEpoch || detached.PlacementState != "lost" {
		t.Fatalf("detach not observable: %+v", detached)
	}
	if _, err := client.PatchIfMatch(ctx, created.SandboxID, paused.MutationRevision, sdk.PatchRequest{Volumes: &empty}); err == nil {
		t.Fatal("old validator changed binding after detach")
	}
	response = do(t, http.MethodGet, srv.URL+"/v1/sandboxes/"+created.SandboxID+"/binding")
	defer response.Body.Close()
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("mutable binding can be cached")
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"overlay", "netns", "host_path", "bundle", "mount_source", "endpoint"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("binding exposed private %s", key)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		response := do(t, method, srv.URL+"/v1/sandboxes/"+created.SandboxID+"/binding")
		response.Body.Close()
		if response.StatusCode != 405 {
			t.Fatalf("binding wrong method %s: %d", method, response.StatusCode)
		}
	}
	response = do(t, http.MethodGet, srv.URL+"/v1/sandboxes/missing/binding")
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatalf("missing binding: %d", response.StatusCode)
	}
}
