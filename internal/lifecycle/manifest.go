package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"github.com/TSHOGX/khora/internal/model"
)

func defaultTemplateManifest() model.TemplateManifest {
	return model.DefaultTemplateManifest()
}

func validateTemplateForLaunch(ctx context.Context, rec model.Sandbox, st interface {
	GetTemplate(context.Context, model.TemplateID) (model.Template, error)
}, runtimeChecker interface {
	CheckManifest(context.Context, string, []string) error
}, digestChecker interface {
	TemplateDigest(context.Context, string) (string, error)
}) error {
	tm, err := st.GetTemplate(ctx, rec.Template)
	if err != nil {
		return err
	}
	manifest := tm.Manifest.Normalize()
	if err := manifest.Validate(); err != nil {
		return err
	}
	if runtimeChecker != nil {
		if err := runtimeChecker.CheckManifest(ctx, manifest.Platform, manifest.Capabilities); err != nil {
			return fmt.Errorf("Template manifest runtime compatibility: %w", err)
		}
	}
	if digestChecker != nil && manifest.ContentDigest != "" {
		got, err := digestChecker.TemplateDigest(ctx, string(rec.Template))
		if err != nil {
			return fmt.Errorf("Template manifest digest: %w", err)
		}
		if !strings.EqualFold(got, manifest.ContentDigest) {
			return fmt.Errorf("Template manifest digest mismatch")
		}
	}
	return nil
}
