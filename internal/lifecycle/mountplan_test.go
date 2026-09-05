package lifecycle

import (
	"errors"
	"testing"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

func TestCompileMountPlanOrdersParentBeforeChild(t *testing.T) {
	rows := []model.Mount{
		{Sandbox: "sbx_a", Volume: "vol_child", Path: "/data/sub/deep", Access: model.SharedReadOnly},
		{Sandbox: "sbx_a", Volume: "vol_root", Path: "/data", Access: model.SharedReadWrite},
		{Sandbox: "sbx_a", Volume: "vol_sibling", Path: "/other", Access: model.SharedReadOnly},
	}
	plan, err := CompileMountPlan(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rows) != 3 || plan.Rows[0].Path != "/data" || plan.Rows[1].Path != "/other" || plan.Rows[2].Path != "/data/sub/deep" {
		t.Fatalf("compiled order = %+v", plan.Rows)
	}
	if rows[0].Path != "/data/sub/deep" {
		t.Fatal("compiler mutated input rows")
	}
}

func TestCompileMountPlanRejectsWritableChildOfReadOnlyParent(t *testing.T) {
	_, err := CompileMountPlan([]model.Mount{
		{Sandbox: "sbx_a", Volume: "vol_parent", Path: "/data", Access: model.SharedReadOnly},
		{Sandbox: "sbx_a", Volume: "vol_child", Path: "/data/sub", Access: model.SharedReadWrite},
	})
	if err == nil || !errors.Is(err, store.ErrMountBindingInvalid) {
		t.Fatalf("unsafe nested plan = %v; want ErrMountBindingInvalid", err)
	}
}
