//go:build substrate

// L2 acceptance for the large-file bypass (the documented invariant fs / transport the documented invariant) against the
// REAL sandboxd under real runsc: streaming upload + download of a payload far
// larger than any sane protobuf message. Requires root.
//
//	sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 ./test/ -run TestFilesBypass
package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"connectrpc.com/connect"
)

// bypassSize is large enough that BUFFERING it is visible as memory. What the
// bypass buys is not a size ceiling — the RPC path accepts a payload this big too
// (Connect's default message limit is generous) — it is that neither end ever
// holds the whole file.
//
// Measured on the guest daemon's own VmRSS for this exact payload:
//
//	baseline  19 MB
//	bypass    28 MB   (+9 MB — streamed)
//	RPC      357 MB   (+338 MB — the whole message, buffered)
//
// So the bypass is ~37× cheaper in peak guest memory, and that ratio grows with
// the file. A sandbox has a RAM cap (the documented invariant) whose breach TERMINATES it, so pushing
// a large file through the RPC path is a way to kill the sandbox you are writing to.
const bypassSize = 64 << 20 // 64 MiB

func TestFilesBypassRoundTrip(t *testing.T) {
	cl, done := launchOne(t)
	defer done()
	ctx := context.Background()

	// A pseudo-random, verifiable payload: a repeating pattern would let a broken
	// implementation truncate and still match on a prefix check.
	payload := make([]byte, bypassSize)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	want := sha256.Sum256(payload)

	const path = "/root/big.bin"

	// --- upload streams (never one message, never wholly in memory) -----------
	n, err := cl.WriteStream(ctx, path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("WriteStream %d bytes: %v", bypassSize, err)
	}
	if n != int64(bypassSize) {
		t.Errorf("WriteStream reported %d bytes; want %d", n, bypassSize)
	}

	// The guest agrees on the size — so the bytes really landed on the guest's own
	// filesystem, not somewhere host-side.
	st, err := cl.Stat(ctx, path)
	if err != nil {
		t.Fatalf("Stat after upload: %v", err)
	}
	if st.Size != int64(bypassSize) {
		t.Fatalf("guest Stat size = %d; want %d — the upload did not fully land", st.Size, bypassSize)
	}

	// --- download streams back, byte-identical --------------------------------
	rc, err := cl.OpenRead(ctx, path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	h := sha256.New()
	got, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("streaming download: %v", err)
	}
	if got != int64(bypassSize) {
		t.Fatalf("downloaded %d bytes; want %d", got, bypassSize)
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	if sum != want {
		t.Fatalf("round-tripped digest %s != %s — the bytes changed in transit",
			hex.EncodeToString(sum[:]), hex.EncodeToString(want[:]))
	}
	t.Logf("✓ %d MiB round-tripped byte-identical over the streaming bypass (sha256 %s…)",
		bypassSize>>20, hex.EncodeToString(want[:8]))

	// --- the RPC path and the bypass see the SAME file ------------------------
	// Not a detail: two paths to one filesystem must not diverge, or a caller's
	// choice of transport would change what it observes.
	head, err := cl.Read(ctx, path, 0, 16)
	if err != nil {
		t.Fatalf("RPC Read of a bypass-uploaded file: %v", err)
	}
	if !bytes.Equal(head, payload[:16]) {
		t.Errorf("RPC Read head = %x; want %x — the two fs paths disagree", head, payload[:16])
	}

	// --- a missing file is not_found on the bypass too ------------------------
	// The bypass maps HTTP status back to the same codes the RPCs use, so the
	// caller gets one error contract regardless of which path carried the bytes.
	if _, err := cl.OpenRead(ctx, "/root/definitely-absent.bin"); err == nil {
		t.Error("OpenRead of a missing file succeeded")
	} else if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("OpenRead missing file: code=%v; want not_found (same contract as the RPC path)", code)
	}

	// --- upload creates parent directories -----------------------------------
	// Matches MakeDir's mkdir -p, so an upload does not fail merely because a
	// directory is missing.
	deep := "/root/nested/deeper/file.bin"
	if _, err := cl.WriteStream(ctx, deep, bytes.NewReader([]byte("small"))); err != nil {
		t.Fatalf("WriteStream into a missing directory: %v; want parents created", err)
	}
	if _, err := cl.Stat(ctx, deep); err != nil {
		t.Errorf("Stat %s after upload: %v", deep, err)
	}
	t.Logf("✓ bypass upload creates parent directories and shares one contract with the RPCs")
}
