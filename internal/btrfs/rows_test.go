package btrfs

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestQuotaRowsRequiresSuccessfulWellFormedRead(t *testing.T) {
	failure := errors.New("read failed")
	for _, tc := range []struct {
		name, out string
		err       error
		bad       bool
	}{
		{"empty", "", nil, true},
		{"command", "qgroupid referenced exclusive max_referenced max_exclusive path\n", failure, true},
		{"absent", "Qgroupid referenced exclusive max_referenced max_exclusive path\n", nil, false},
		{"invalid id", "qgroupid referenced exclusive max_referenced max_exclusive path\n0/nope 0 0 none none <stale>\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Client{Run: func(context.Context, []string) ([]byte, error) { return []byte(tc.out), tc.err }}
			rows, err := c.QuotaRows(context.Background(), "mount", []uint64{42})
			if (err != nil) != tc.bad {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatal("lost read error")
			}
			if !tc.bad && rows[42] != RowAbsent {
				t.Fatal(rows)
			}
		})
	}
}

func TestQuotaRowsOnlyClassifiesRecordedIDs(t *testing.T) {
	c := Client{Run: func(context.Context, []string) ([]byte, error) {
		return []byte("qgroupid referenced exclusive max_referenced max_exclusive path\n0/1 0 0 none none /live/<stale>\n0/2 0 0 none none <stale>\n0/3 0 0 none none <future state>\n0/4 0\n0/99 0 0 none none <stale>\n"), nil
	}}
	rows, err := c.QuotaRows(context.Background(), "mount", []uint64{1, 2, 3, 4, 5})
	want := map[uint64]QuotaRowState{1: RowLive, 2: RowStale, 3: RowUnknown, 4: RowUnknown, 5: RowAbsent}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}
