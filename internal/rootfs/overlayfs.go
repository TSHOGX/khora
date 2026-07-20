package rootfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// HostOverlayFS is the M1 single-node FS provider (§5.B): it assembles the
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
	// TemplatesDir resolves a TemplateID to its read-only lower dir:
	// TemplatesDir/<template>. Templates are static directories in M1 (§6, no
	// Promote / layering until M6).
	TemplatesDir string
}

// NewHostOverlayFS constructs the provider, ensuring StateDir exists.
func NewHostOverlayFS(stateDir, templatesDir string) (*HostOverlayFS, error) {
	if stateDir == "" || templatesDir == "" {
		return nil, fmt.Errorf("rootfs: StateDir and TemplatesDir are required")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create StateDir: %w", err)
	}
	return &HostOverlayFS{StateDir: stateDir, TemplatesDir: templatesDir}, nil
}

func (p *HostOverlayFS) Name() string { return "host_overlayfs" }

// --- deterministic path derivation (host paths, private to this subsystem) ---

func (p *HostOverlayFS) upperPath(id string) string  { return filepath.Join(p.StateDir, id, "upper") }
func (p *HostOverlayFS) workPath(id string) string   { return filepath.Join(p.StateDir, id, "work") }
func (p *HostOverlayFS) mergedPath(id string) string { return filepath.Join(p.StateDir, id, "merged") }
func (p *HostOverlayFS) lowerPath(template string) string {
	return filepath.Join(p.TemplatesDir, template)
}

// handle rebuilds the private Overlay struct fields from the ID.
func (p *HostOverlayFS) handle(overlayID, template string) *Overlay {
	return &Overlay{
		ID:        overlayID,
		Template:  template,
		upperPath: p.upperPath(overlayID),
		workPath:  p.workPath(overlayID),
	}
}

// CreateOverlay allocates an empty upper (① create prefix). It does NOT mount.
func (p *HostOverlayFS) CreateOverlay(_ context.Context, overlayID, template string) (*Overlay, error) {
	if err := p.validate(overlayID, template); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.lowerPath(template)); err != nil {
		return nil, fmt.Errorf("rootfs: Template %q not found: %w", template, err)
	}
	if err := os.MkdirAll(p.upperPath(overlayID), 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create upper: %w", err)
	}
	return p.handle(overlayID, template), nil
}

// OpenOverlay reconstructs a handle for an existing artifact (resume prefix).
func (p *HostOverlayFS) OpenOverlay(_ context.Context, overlayID, template string) (*Overlay, error) {
	if err := p.validate(overlayID, template); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.upperPath(overlayID)); err != nil {
		return nil, fmt.Errorf("rootfs: Overlay %q upper missing (not created?): %w", overlayID, err)
	}
	return p.handle(overlayID, template), nil
}

// Mount assembles lower(Template)/upper/work → merged (① of the cold-start
// chain). A FRESH empty workdir is required per (re)mount — the kernel rejects
// or corrupts a reused work (rootfs-overlay D5). resume re-mounts the SAME
// upper (pause captured nothing, §4.2).
func (p *HostOverlayFS) Mount(_ context.Context, o *Overlay) (*Merged, error) {
	id := o.ID
	lower := p.lowerPath(o.Template)
	upper := p.upperPath(id)
	work := p.workPath(id)
	merged := p.mergedPath(id)

	if _, err := os.Stat(lower); err != nil {
		return nil, fmt.Errorf("rootfs: lower Template missing: %w", err)
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

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
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

// DestroyOverlay reclaims upper+work bytes (terminal destroy, §4.1). M1 deletes
// synchronously (M2 → refcount async sweep). Separate from placement teardown;
// the mediator invokes each (§4 subsystems don't call each other).
func (p *HostOverlayFS) DestroyOverlay(_ context.Context, o *Overlay) error {
	dir := filepath.Join(p.StateDir, o.ID)
	// best-effort unmount first in case merged is still mounted.
	_ = syscall.Unmount(p.mergedPath(o.ID), syscall.MNT_DETACH)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("rootfs: destroy overlay %s: %w", o.ID, err)
	}
	return nil
}

// validate keeps IDs to a safe single path segment (no traversal / separators).
func (p *HostOverlayFS) validate(overlayID, template string) error {
	for name, v := range map[string]string{"overlayID": overlayID, "template": template} {
		if v == "" {
			return fmt.Errorf("rootfs: empty %s", name)
		}
		if strings.ContainsAny(v, "/\\") || v == "." || v == ".." {
			return fmt.Errorf("rootfs: unsafe %s %q", name, v)
		}
	}
	return nil
}
