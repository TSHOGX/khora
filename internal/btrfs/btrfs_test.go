package btrfs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUsageRetryBoundary(t *testing.T) {
	t.Run("command failure succeeds on final attempt", func(t *testing.T) {
		calls := 0
		c := Client{Run: func(context.Context, []string) ([]byte, error) {
			calls++
			if calls < usageReadAttempts {
				return nil, errors.New("transient qgroup read")
			}
			return []byte("qgroupid referenced exclusive max_referenced max_exclusive path\n" +
				"0/42 4096 4096 8192 none /state/unit\n"), nil
		}}

		used, limit, err := c.Usage(context.Background(), "/state/unit")
		if err != nil {
			t.Fatalf("Usage: %v", err)
		}
		if calls != usageReadAttempts {
			t.Fatalf("qgroup reads = %d, want %d", calls, usageReadAttempts)
		}
		if used != 4096 || limit != 8192 {
			t.Fatalf("Usage = (%d, %d), want (4096, 8192)", used, limit)
		}
	})

	t.Run("command failure stops at the bound", func(t *testing.T) {
		calls := 0
		c := Client{Run: func(context.Context, []string) ([]byte, error) {
			calls++
			return nil, errors.New("persistent qgroup read")
		}}

		_, _, err := c.Usage(context.Background(), "/state/unit")
		if err == nil {
			t.Fatal("Usage succeeded after every qgroup read failed")
		}
		if calls != usageReadAttempts {
			t.Fatalf("qgroup reads = %d, want %d", calls, usageReadAttempts)
		}
		if !strings.Contains(err.Error(), "(3 attempts)") {
			t.Fatalf("error does not report the retry bound: %v", err)
		}
	})

	t.Run("successful response without a row is not retried", func(t *testing.T) {
		calls := 0
		c := Client{Run: func(context.Context, []string) ([]byte, error) {
			calls++
			return []byte("qgroupid referenced exclusive max_referenced max_exclusive path\n"), nil
		}}

		_, _, err := c.Usage(context.Background(), "/state/unit")
		if err == nil || !strings.Contains(err.Error(), "no qgroup row") {
			t.Fatalf("Usage error = %v, want missing-row error", err)
		}
		if calls != 1 {
			t.Fatalf("semantic failure made %d qgroup reads, want 1", calls)
		}
	})
}
