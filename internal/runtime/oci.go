package runtime

// Minimal OCI runtime-spec (config.json) builder. We emit only the fields runsc
// needs (§5.A / rootfs-overlay), not the full runtime-spec — pulling the whole
// opencontainers/runtime-spec module in for a handful of fields would be weight
// §1.1 rejects.

type ociSpec struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname"`
	Mounts     []ociMount `json:"mounts"`
	Linux      ociLinux   `json:"linux"`
}

type ociProcess struct {
	Terminal     bool            `json:"terminal"`
	User         ociUser         `json:"user"`
	Args         []string        `json:"args"`
	Env          []string        `json:"env"`
	Cwd          string          `json:"cwd"`
	Capabilities ociCapabilities `json:"capabilities"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ociCapabilities struct {
	Bounding  []string `json:"bounding"`
	Effective []string `json:"effective"`
	Permitted []string `json:"permitted"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces"`
}

type ociNamespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// caps is the guest capability set: enough for sandboxd to bind a netns TCP
// port and exec processes. Egress steering / dropping down to a minimal set
// arrives with the network subsystem; today the netns is the simplest reachable
// one.
var caps = []string{
	"CAP_NET_BIND_SERVICE", "CAP_NET_ADMIN", "CAP_NET_RAW",
	"CAP_SYS_ADMIN", "CAP_CHOWN", "CAP_DAC_OVERRIDE",
	"CAP_KILL", "CAP_SETUID", "CAP_SETGID", "CAP_FOWNER",
}

// buildOCISpec returns the config.json for a merged rootfs + adopted netns.
// entrypoint is sandboxd (init mode → PID 1, §5.E.1). netnsPath is the
// engine-pre-built netns runsc adopts (§5.A: --network=sandbox only adopts).
func buildOCISpec(mergedPath, netnsPath string) *ociSpec {
	return &ociSpec{
		OCIVersion: "1.0.0",
		Process: ociProcess{
			Terminal: false,
			// Guest root (uid/gid 0). This is the engine default, not a stub: guest
			// identity is not a host-isolation pillar (§2.2 — sentry virtualizes guest
			// uid 0, sentry is unprivileged on the host), so guest-root costs nothing
			// on the isolation axis and is the most direct expression of §1.1's
			// "root-capable agent sandbox". PID 1 is sandboxd init; it stays root so
			// it can hold CAP_NET_* for the netns transport port (§5.A).
			User:     ociUser{UID: 0, GID: 0},
			Args:     []string{"/sbin/sandboxd", "init"},
			Env:      []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"},
			Cwd:      "/",
			Capabilities: ociCapabilities{
				Bounding: caps, Effective: caps, Permitted: caps,
			},
		},
		Root:     ociRoot{Path: mergedPath, Readonly: false},
		Hostname: "guest",
		Mounts: []ociMount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs",
				Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "nodev", "mode=1777"}},
		},
		Linux: ociLinux{
			Namespaces: []ociNamespace{
				{Type: "pid"}, {Type: "mount"}, {Type: "ipc"}, {Type: "uts"},
				{Type: "network", Path: netnsPath},
			},
		},
	}
}
