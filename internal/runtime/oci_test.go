package runtime

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/guestport"
)

func TestProcessEnvironmentCarriesConfiguredRetention(t *testing.T) {
	got, err := processEnvironment(37 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The wire value is nanoseconds, not a human duration string: it is
	// unambiguous across the runtime/guest boundary and cannot be locale parsed.
	want := guestport.ProcessRetentionEnv + "=" + strconv.FormatInt(int64(37*time.Minute), 10)
	if len(got) != 3 || got[2] != want {
		t.Fatalf("process environment = %v; want retention entry %q", got, want)
	}
}

func TestProcessEnvironmentOmitsZeroRetention(t *testing.T) {
	got, err := processEnvironment(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range got {
		if strings.HasPrefix(entry, guestport.ProcessRetentionEnv+"=") {
			t.Fatalf("zero retention unexpectedly crossed launch environment: %v", got)
		}
	}
}

func TestProcessEnvironmentRejectsNegativeRetention(t *testing.T) {
	if _, err := processEnvironment(-time.Second); err == nil {
		t.Fatal("negative retention unexpectedly crossed launch environment")
	}
}
