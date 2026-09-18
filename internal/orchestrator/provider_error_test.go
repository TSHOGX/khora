package orchestrator

import (
	"fmt"
	"github.com/TSHOGX/khora/internal/volume"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderFailurePrecedesFilesystemCause(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("quota state /srv/private: %w: %w", volume.ErrProviderUnavailable, fs.ErrPermission), 503},
		{fmt.Errorf("quota state /srv/private: %w: %w", volume.ErrProviderUnavailable, fs.ErrNotExist), 503},
		{fmt.Errorf("open /srv/private: %w", fs.ErrPermission), 403},
	} {
		rr := httptest.NewRecorder()
		statusFor(rr, tc.err)
		if rr.Code != tc.status || strings.Contains(rr.Body.String(), "/srv/") {
			t.Fatalf("response: %d %s", rr.Code, rr.Body.String())
		}
	}
}
