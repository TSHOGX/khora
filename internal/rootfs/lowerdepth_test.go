// MaxLowerDepth is pure arithmetic over this provider's own configured paths — no mount, no
// root — so it runs in the default unit suite. Mount's exact check is the substrate half
// (overlayfs_substrate_test.go).
//
// What is being guarded: the lower stack reaches mount(2) as one colon-separated `lowerdir=`
// string, and the classic mount path TRUNCATES options past a page rather than refusing
// (rootfs-overlay D5.1 ⑩). A stack at or under the reported depth must therefore always fit,
// which makes UNDER-reporting safe and OVER-reporting a silent data-loss bug.
package rootfs

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The reported depth must actually fit, including the worst case the number promises:
// every layer under the LONGER of the two roots, every ID at full width. Building that
// worst-case options string and measuring it is the only check that cannot drift from
// Mount's own arithmetic.
func TestMaxLowerDepthWorstCaseStackFitsTheOptionsCap(t *testing.T) {
	for _, p := range []*HostOverlayFS{
		{StateDir: "/var/lib/khora/overlays", LayersDir: "/var/lib/khora/layers", TemplatesDir: "/var/lib/khora/templates"},
		// TemplatesDir longer than LayersDir: the guarantee must hold whichever root a
		// layer resolves under, since layerPath picks per ID.
		{StateDir: "/s", LayersDir: "/l", TemplatesDir: "/var/lib/khora/deployment/templates/builtin"},
		// A deep StateDir eats the fixed overhead instead of the per-entry budget.
		{StateDir: "/var/lib/khora/very/deeply/nested/state/directory/for/overlays", LayersDir: "/var/lib/khora/layers", TemplatesDir: "/t"},
	} {
		depth, err := p.MaxLowerDepth(context.Background())
		if err != nil {
			t.Fatalf("MaxLowerDepth(%s): %v", p.StateDir, err)
		}
		if depth <= 0 {
			t.Fatalf("MaxLowerDepth(%s) = %d; want a positive depth", p.StateDir, depth)
		}

		// The worst case the number promises: longest root, full-width IDs.
		root := p.LayersDir
		if len(p.TemplatesDir) > len(root) {
			root = p.TemplatesDir
		}
		id := strings.Repeat("x", maxIDLen)
		lowerDirs := make([]string, 0, depth)
		for i := 0; i < depth; i++ {
			lowerDirs = append(lowerDirs, filepath.Join(root, id))
		}
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			strings.Join(lowerDirs, ":"),
			filepath.Join(p.StateDir, id, "upper"),
			filepath.Join(p.StateDir, id, "work"))

		if len(opts) > mountOptionsCap {
			t.Errorf("MaxLowerDepth(%s) = %d, but a worst-case stack of that depth needs %d bytes of mount options against a %d-byte cap — the kernel would TRUNCATE it and mount a stack missing its tail (D5.1 ⑩)",
				p.StateDir, depth, len(opts), mountOptionsCap)
		}
	}
}

// A ceiling this small is what a misconfigured deployment produces, and it must be an
// error rather than a 0 that silently reads as "no limit".
func TestMaxLowerDepthRefusesPathsTooLongToMountAnything(t *testing.T) {
	long := "/" + strings.Repeat("d", mountOptionsCap)
	for name, p := range map[string]*HostOverlayFS{
		"state dir over the cap": {StateDir: long, LayersDir: "/l", TemplatesDir: "/t"},
		"layer dir over the cap": {StateDir: "/s", LayersDir: long, TemplatesDir: "/t"},
	} {
		if _, err := p.MaxLowerDepth(context.Background()); err == nil {
			t.Errorf("%s: MaxLowerDepth succeeded; want an error — reporting 0 here would read as NO LIMIT and admit every stack", name)
		}
	}
}
