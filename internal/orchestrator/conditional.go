package orchestrator

import (
	"encoding/hex"
	"net/http"

	"github.com/TSHOGX/khora/internal/model"
)

// The mutation revision is a single strong validator, not a wildcard or list.
// Leaving If-Match absent preserves unconditional operator verbs.
func mutationPrecondition(r *http.Request) (string, error) {
	if len(r.Header.Values("If-Match")) > 1 {
		return "", model.NewContractError(model.ErrorInvalidArgument, "If-Match requires one mutation revision")
	}
	value := r.Header.Get("If-Match")
	if value == "" {
		return "", nil
	}
	if len(value) != 66 || value[0] != '"' || value[65] != '"' {
		return "", model.NewContractError(model.ErrorInvalidArgument, "If-Match requires one quoted mutation revision")
	}
	revision := value[1:65]
	if _, err := hex.DecodeString(revision); err != nil {
		return "", model.NewContractError(model.ErrorInvalidArgument, "invalid mutation revision")
	}
	return revision, nil
}
