package rootfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// HostOverlayFS is the single-node FS provider (§5.B): it assembles the
// sandbox's writable Overlay artifact as a host kernel overlayfs — lower =
// Template, upper+work = the resident Overlay directory, merged = the rootfs
// handed to runsc (rootfs-overlay D0/D3). runsc runs --overlay2=none
// --file-access=shared and never manages the write layer.
//
// Host paths are LOCKED inside this subsystem (§3.1.1 对内也 ID 寻址): the
// Overlay's upper/work/merged all derive deterministically from the OverlayID
// under StateDir, so the handle is reconstructable from a store record on
// resume (OpenOverlay) without keeping it in memory. Above this subsystem an
// Overlay is only {ID, Template}.
//
// mount/umount are privileged (CAP_SYS_ADMIN) and copy-up files are root-owned
// (rootfs-overlay D5); the orchestrator therefore runs as root (ADR-0002).
// That privilege, and the host paths, stay sealed here — never in the mediator.
type HostOverlayFS struct {
	// StateDir holds per-Overlay materialization: StateDir/<id>/{upper,work,merged}.
	StateDir string
	// TemplatesDir resolves a BUILTIN TemplateID to its read-only lower dir:
	// TemplatesDir/<template>. Builtin templates are static directories shipped
	// with the deployment (boot seed, §4.3); they are deployment assets, never swept.
	TemplatesDir string
	// LayersDir holds Promote-built layer directories: LayersDir/<templateID>.
	// These are the immutable read-only layers a Promote freezes (§5.B) — the
	// bytes-first artifact whose orphans (no live templates row) the sweep
	// reclaims (§5.B / §4.4-A). Distinct from TemplatesDir so the sweep never touches a
	// deployment-shipped builtin.
	LayersDir string
}

// NewHostOverlayFS constructs the provider, ensuring StateDir + LayersDir exist.
func NewHostOverlayFS(stateDir, templatesDir, layersDir string) (*HostOverlayFS, error) {
	if stateDir == "" || templatesDir == "" || layersDir == "" {
		return nil, fmt.Errorf("rootfs: StateDir, TemplatesDir and LayersDir are required")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create StateDir: %w", err)
	}
	if err := os.MkdirAll(layersDir, 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create LayersDir: %w", err)
	}
	return &HostOverlayFS{StateDir: stateDir, TemplatesDir: templatesDir, LayersDir: layersDir}, nil
}

func (p *HostOverlayFS) Name() string { return "host_overlayfs" }

// --- deterministic path derivation (host paths, private to this subsystem) ---

func (p *HostOverlayFS) upperPath(id string) string  { return filepath.Join(p.StateDir, id, "upper") }
func (p *HostOverlayFS) workPath(id string) string   { return filepath.Join(p.StateDir, id, "work") }
func (p *HostOverlayFS) mergedPath(id string) string { return filepath.Join(p.StateDir, id, "merged") }

// layerPath resolves a lower-stack ID to its on-disk read-only layer dir. A
// layer is EITHER a Promote-built layer under LayersDir OR a deployment-shipped
// builtin under TemplatesDir; we prefer LayersDir (Promote layers) and fall back
// to TemplatesDir (builtins). Both roots are private to this subsystem (§3.1.1);
// the caller only ever sees the opaque ID.
func (p *HostOverlayFS) layerPath(id string) (string, error) {
	if lp := filepath.Join(p.LayersDir, id); dirExists(lp) {
		return lp, nil
	}
	if tp := filepath.Join(p.TemplatesDir, id); dirExists(tp) {
		return tp, nil
	}
	return "", fmt.Errorf("rootfs: layer %q not found under layers or templates", id)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// handle builds the above-subsystem Overlay view ({ID, Template, Lower}); host
// paths are derived on demand from the IDs, not stored (see rootfs.go).
func (p *HostOverlayFS) handle(overlayID string, lower []string) *Overlay {
	tmpl := ""
	if len(lower) > 0 {
		tmpl = lower[0]
	}
	return &Overlay{ID: overlayID, Template: tmpl, Lower: append([]string(nil), lower...)}
}

// CreateOverlay allocates an empty upper over a read-only lower stack (① create
// prefix). It does NOT mount.
func (p *HostOverlayFS) CreateOverlay(_ context.Context, overlayID string, lower []string) (*Overlay, error) {
	if err := p.validateStack(overlayID, lower); err != nil {
		return nil, err
	}
	for _, id := range lower {
		if _, err := p.layerPath(id); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(p.upperPath(overlayID), 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create upper: %w", err)
	}
	return p.handle(overlayID, lower), nil
}

// OpenOverlay reconstructs a handle for an existing artifact (resume prefix).
func (p *HostOverlayFS) OpenOverlay(_ context.Context, overlayID string, lower []string) (*Overlay, error) {
	if err := p.validateStack(overlayID, lower); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.upperPath(overlayID)); err != nil {
		return nil, fmt.Errorf("rootfs: Overlay %q upper missing (not created?): %w", overlayID, err)
	}
	return p.handle(overlayID, lower), nil
}

// Mount assembles lower(Template)/upper/work → merged (① of the cold-start
// chain). A FRESH empty workdir is required per (re)mount — the kernel rejects
// or corrupts a reused work (rootfs-overlay D5). resume re-mounts the SAME
// upper (pause captured nothing, §4.2).
func (p *HostOverlayFS) Mount(_ context.Context, o *Overlay) (*Merged, error) {
	id := o.ID
	upper := p.upperPath(id)
	work := p.workPath(id)
	merged := p.mergedPath(id)

	// Resolve the lower stack to host dirs. overlayfs lowerdir is COLON-separated,
	// leaf-first (lowerdir=leaf:...:base) — the SAME leaf-first order as o.Lower
	// (§5.B multi-lowerdir: 新 Template 的 lower 栈 = [新层, ...父栈]).
	lowerDirs := make([]string, 0, len(o.Lower))
	for _, lid := range o.Lower {
		lp, err := p.layerPath(lid)
		if err != nil {
			return nil, fmt.Errorf("rootfs: mount lower: %w", err)
		}
		lowerDirs = append(lowerDirs, lp)
	}
	if len(lowerDirs) == 0 {
		return nil, fmt.Errorf("rootfs: mount overlay %s: empty lower stack", id)
	}

	// fresh workdir each mount.
	if err := os.RemoveAll(work); err != nil {
		return nil, fmt.Errorf("rootfs: clear stale work: %w", err)
	}
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("rootfs: mkdir %s: %w", d, err)
		}
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lowerDirs, ":"), upper, work)
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return nil, fmt.Errorf("rootfs: mount overlay at %s (%s): %w", merged, opts, err)
	}
	return &Merged{MergedPath: merged}, nil
}

// Unmount tears down the merged overlay (pause / destroy). upper persists
// across pause (§4.2 零捕获).
func (p *HostOverlayFS) Unmount(_ context.Context, m *Merged) error {
	if m == nil || m.MergedPath == "" {
		return nil
	}
	if err := syscall.Unmount(m.MergedPath, 0); err != nil {
		// MNT_DETACH fallback for a busy mount (lazy unmount, cleaned when refs drop).
		if err2 := syscall.Unmount(m.MergedPath, syscall.MNT_DETACH); err2 != nil {
			return fmt.Errorf("rootfs: unmount %s: %w (detach: %v)", m.MergedPath, err, err2)
		}
	}
	return nil
}

// DestroyOverlay reclaims upper+work bytes (terminal destroy, §4.1). Deletes
// synchronously; a refcount-driven async sweep is the eventual form. Separate
// from placement teardown; the mediator invokes each (§4 subsystems don't call
// each other).
func (p *HostOverlayFS) DestroyOverlay(_ context.Context, o *Overlay) error {
	dir := filepath.Join(p.StateDir, o.ID)
	// best-effort unmount first in case merged is still mounted.
	_ = syscall.Unmount(p.mergedPath(o.ID), syscall.MNT_DETACH)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("rootfs: destroy overlay %s: %w", o.ID, err)
	}
	return nil
}

// AdoptMerged reconstructs the merged handle for an Overlay still mounted after
// a crash (§4.4-A). The merged dir is deterministic from the ID; we confirm it
// is actually an overlay mount (kernel state held by the surviving container)
// before handing back the handle — a not-mounted merged means the container is
// not really live, so the caller should not adopt it.
func (p *HostOverlayFS) AdoptMerged(_ context.Context, o *Overlay) (*Merged, error) {
	merged := p.mergedPath(o.ID)
	mounted, err := isMountpoint(merged)
	if err != nil {
		return nil, fmt.Errorf("rootfs: adopt merged %s: %w", o.ID, err)
	}
	if !mounted {
		return nil, fmt.Errorf("rootfs: overlay %s not mounted (container not live)", o.ID)
	}
	return &Merged{MergedPath: merged}, nil
}

// ListBuiltinTemplates enumerates the static Template directories under
// TemplatesDir (boot seed, §4.3). Each subdirectory name is a builtin TemplateID;
// its bytes are the read-only lower shipped with the deployment (bytes-first is
// inherent — the dir exists before any row). The host TemplatesDir path never
// leaves this subsystem — only the opaque IDs (§3.1.1). A missing TemplatesDir
// is treated as "no builtins" (empty), not an error: a deployment may ship only
// Promote-built Templates.
func (p *HostOverlayFS) ListBuiltinTemplates(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(p.TemplatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("rootfs: list templates: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// SweepOrphanLayers reclaims Promote-built layer dirs under LayersDir with no
// live `templates` row (§5.B / §4.4-A). It reuses the §4.4-A reverse-reconcile shape: the
// authoritative set (`live` + `building`) is passed in, the provider diffs it
// against what is on disk and deletes the difference. `building` protects an
// in-flight Promote whose bytes are down but whose row is not yet inserted
// (bytes-first, §5.B) — without it a concurrent sweep would tear the layer out
// from under the Promote. A missing LayersDir → nothing to sweep.
func (p *HostOverlayFS) SweepOrphanLayers(_ context.Context, live, building []string) (int, error) {
	keep := make(map[string]bool, len(live)+len(building))
	for _, id := range live {
		keep[id] = true
	}
	for _, id := range building {
		keep[id] = true
	}
	entries, err := os.ReadDir(p.LayersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("rootfs: sweep list layers: %w", err)
	}
	reclaimed := 0
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		// orphan: a layer dir with no live row and not being built → reclaim its
		// bytes (§5.B / §4.4-A). The row-vs-bytes decoupling means the row is already gone
		// (DELETE template) or never landed (crashed Promote); either way the
		// bytes are safe to drop.
		if err := os.RemoveAll(filepath.Join(p.LayersDir, e.Name())); err != nil {
			return reclaimed, fmt.Errorf("rootfs: sweep reclaim layer %s: %w", e.Name(), err)
		}
		reclaimed++
	}
	return reclaimed, nil
}

// isMountpoint reports whether path is a mount point by comparing its device
// number to its parent's (a mount boundary changes st_dev). Cheap, no /proc
// parse, correct for overlayfs.
func isMountpoint(path string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return st.Dev != parent.Dev, nil
}

// validateStack keeps the overlay ID and every lower-stack ID to a safe single
// path segment (no traversal / separators), and requires a non-empty stack.
func (p *HostOverlayFS) validateStack(overlayID string, lower []string) error {
	if err := validateSeg("overlayID", overlayID); err != nil {
		return err
	}
	if len(lower) == 0 {
		return fmt.Errorf("rootfs: empty lower stack for overlay %q", overlayID)
	}
	for _, id := range lower {
		if err := validateSeg("lower", id); err != nil {
			return err
		}
	}
	return nil
}

// validateSeg rejects an empty or path-unsafe single segment (also used by
// PromoteToTemplate for the new layer ID).
func validateSeg(name, v string) error {
	if v == "" {
		return fmt.Errorf("rootfs: empty %s", name)
	}
	if strings.ContainsAny(v, "/\\") || v == "." || v == ".." {
		return fmt.Errorf("rootfs: unsafe %s %q", name, v)
	}
	return nil
}

// promoteExcludedPaths is the minimal excluded-path set (§5.B / §3.1 Promote;
// rootfs-overlay D5): engine-injected guest-runtime paths that must NOT be
// captured into a Promoted layer. Today these are the sandboxd transport portal
// files, written upper-relative (the guest daemon writes them under /tmp, which
// is the upper's tmp/). These MUST track the guest daemon's own paths in
// cmd/sandboxd/transport.go — a rename there without a matching edit here leaks
// a runtime file into a frozen layer (no compile coupling: separate binaries).
// Volume mount-point placeholders join this set when Volume lands; the full list
// is tracked in rootfs-overlay D5.
var promoteExcludedPaths = []string{
	"tmp/.sandboxd-transport.pid", // transport child pidfile (§5.E.1)
	"tmp/.sandboxd-crashloop",     // PID1 crash-loop sentinel (§5.E.1)
}

// PromoteToTemplate freezes the source Overlay's at-rest upper into a new
// read-only layer dir under LayersDir (§5.B Promote). bytes-first: this writes
// ONLY bytes — the caller inserts the templates row afterward (§4.3 INV-3). The
// copy excludes the engine-injected runtime paths (§5.B / §3.1; rootfs-overlay D5). The caller must have
// quiesced + unmounted the sandbox first, so upper is a stable plain dir tree
// (§2.5). The layer is built at a staging path then atomically renamed into
// place, so a crash mid-copy never leaves a half-written layer at the final ID
// (a staging dir is a plain orphan the sweep reclaims).
func (p *HostOverlayFS) PromoteToTemplate(_ context.Context, o *Overlay, newTemplateID string) error {
	if err := validateSeg("newTemplateID", newTemplateID); err != nil {
		return err
	}
	upper := p.upperPath(o.ID)
	if _, err := os.Stat(upper); err != nil {
		return fmt.Errorf("rootfs: promote: source upper missing: %w", err)
	}
	dst := filepath.Join(p.LayersDir, newTemplateID)
	if dirExists(dst) {
		return fmt.Errorf("rootfs: promote: layer %q already exists", newTemplateID)
	}
	// stage under a sibling temp dir on the SAME FS (LayersDir) so the final
	// rename is atomic. A crash before the rename leaves the staging dir; it is
	// NOT at the final ID, so no row will ever reference it → sweep reclaims it.
	staging := dst + ".staging"
	_ = os.RemoveAll(staging)
	excluded := make(map[string]bool, len(promoteExcludedPaths))
	for _, e := range promoteExcludedPaths {
		excluded[filepath.Clean(e)] = true
	}
	if err := copyTree(upper, staging, excluded); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("rootfs: promote: copy upper: %w", err)
	}
	if err := os.Rename(staging, dst); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("rootfs: promote: finalize layer: %w", err)
	}
	return nil
}

// copyTree recursively copies src into dst, skipping paths (relative to src,
// cleaned) present in excluded. Preserves mode bits and symlinks. dst is created.
// Files are copied byte-for-byte (the layer is a plain read-only dir tree, §5.B).
func copyTree(src, dst string, excluded map[string]bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel != "." && excluded[filepath.Clean(rel)] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			// skip devices/sockets/pipes — a frozen read-only layer holds only a
			// normal file tree (an engine-injected socket is excluded anyway).
			return nil
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
