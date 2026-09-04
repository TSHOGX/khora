package orchestrator

import (
	"context"
	"testing"

	"connectrpc.com/connect"
)

func TestProcessProxyRejectsNilRequests(t *testing.T) {
	p := &processProxy{}
	ctx := context.Background()
	if err := p.Start(ctx, nil, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Start(nil) = %v; want invalid_argument", err)
	}
	if err := p.Connect(ctx, nil, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Connect(nil) = %v; want invalid_argument", err)
	}
	if _, err := p.StreamInput(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("StreamInput(nil) = %v; want invalid_argument", err)
	}
	if _, err := p.SendInput(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("SendInput(nil) = %v; want invalid_argument", err)
	}
	if _, err := p.SendSignal(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("SendSignal(nil) = %v; want invalid_argument", err)
	}
	if _, err := p.Update(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Update(nil) = %v; want invalid_argument", err)
	}
	if _, err := p.CloseStdin(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CloseStdin(nil) = %v; want invalid_argument", err)
	}
	if _, err := p.List(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("List(nil) = %v; want invalid_argument", err)
	}
}

func TestFSProxyRejectsNilRequests(t *testing.T) {
	f := &fsProxy{}
	ctx := context.Background()
	if _, err := f.Read(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Read(nil) = %v; want invalid_argument", err)
	}
	if _, err := f.Write(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Write(nil) = %v; want invalid_argument", err)
	}
	if _, err := f.Stat(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Stat(nil) = %v; want invalid_argument", err)
	}
	if _, err := f.List(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("List(nil) = %v; want invalid_argument", err)
	}
	if _, err := f.MakeDir(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("MakeDir(nil) = %v; want invalid_argument", err)
	}
	if _, err := f.Remove(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Remove(nil) = %v; want invalid_argument", err)
	}
	if _, err := f.Move(ctx, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Move(nil) = %v; want invalid_argument", err)
	}
	if err := f.Watch(ctx, nil, nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Watch(nil) = %v; want invalid_argument", err)
	}
}
