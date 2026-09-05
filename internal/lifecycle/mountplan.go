package lifecycle

import (
	"fmt"
	"sort"
	"strings"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

// MountPlan is the validated, deterministic realization order for a complete
// mount binding. Parent destinations precede child destinations so a runtime
// receives the same topology on every placement.
type MountPlan struct {
	Rows []model.Mount
}

func CompileMountPlan(rows []model.Mount) (MountPlan, error) {
	copyRows := append([]model.Mount(nil), rows...)
	if err := validateMountPlanRows(copyRows); err != nil {
		return MountPlan{}, err
	}
	sort.SliceStable(copyRows, func(i, j int) bool {
		di, dj := mountDepth(copyRows[i].Path), mountDepth(copyRows[j].Path)
		if di != dj {
			return di < dj
		}
		return copyRows[i].Path < copyRows[j].Path
	})
	return MountPlan{Rows: copyRows}, nil
}

func validateMountPlanRows(rows []model.Mount) error {
	// Reuse the store's endpoint, duplicate, and normalization checks. The
	// compiler adds only realization-order rules; it never weakens the store's
	// atomic binding validation.
	if err := store.ValidateMountBinding(rows); err != nil {
		return err
	}
	for i, parent := range rows {
		for j, child := range rows {
			if i == j || !strings.HasPrefix(child.Path, parent.Path+"/") {
				continue
			}
			if parent.Access == model.SharedReadOnly && !child.Access.ReadOnly() {
				return fmt.Errorf("%w: writable child %q is inside read-only parent %q", store.ErrMountBindingInvalid, child.Path, parent.Path)
			}
		}
	}
	return nil
}

func mountDepth(p string) int {
	return len(strings.Split(strings.Trim(p, "/"), "/"))
}
