package btrfs

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func TestQuotaStatusFlags(t *testing.T) {
	for _, tc := range []struct {
		flags uint64
		mode  QuotaMode
		bad   bool
	}{
		{0, QuotaDisabled, false}, {1, QuotaUnsupported, false}, {9, QuotaSimple, false}, {11, QuotaUnknown, true}, {13, QuotaUnknown, true}, {17, QuotaUnknown, true},
	} {
		item := make([]byte, 40)
		binary.LittleEndian.PutUint64(item, 1)
		binary.LittleEndian.PutUint64(item[16:], tc.flags)
		mode, err := quotaMode(item)
		if mode != tc.mode || (err != nil) != tc.bad {
			t.Fatalf("flags %d: %v %v", tc.flags, mode, err)
		}
	}
	if _, err := quotaMode(make([]byte, 32)); err == nil {
		t.Fatal("unknown version accepted")
	}
	if _, err := quotaMode(nil); err == nil {
		t.Fatal("truncated status accepted")
	}
}

func TestQuotaProbeFailureIsNotDisabled(t *testing.T) {
	denied := errors.New("permission denied")
	c := Client{ProbeQuota: func(context.Context, string) (QuotaMode, error) { return QuotaUnknown, denied }}
	mode, err := c.QuotaStatus(context.Background(), "ignored")
	if mode != QuotaUnknown || !errors.Is(err, denied) || c.Available(context.Background(), "ignored") {
		t.Fatal("unknown quota state became available")
	}
}
