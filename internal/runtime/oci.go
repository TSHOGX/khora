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
	// CgroupsPath MUST be set explicitly whenever we emit resources (§5.A): omit
	// it and runsc builds its leaf at the cgroup v2 ROOT, polluting the host
	// namespace. It is NOT a host primitive crossing the seam — local_runsc
	// synthesizes it internally from the cid, exactly like netns/cid names.
	CgroupsPath string `json:"cgroupsPath,omitempty"`
	// Resources carries the §3.4 rate + RAM Binding translated into OCI form.
	// omitempty: a sandbox with no limits emits no resources block at all, so
	// runsc creates no cgroup leaf and behaves exactly as before M6.
	Resources *ociResources `json:"resources,omitempty"`
}

// ociResources is the §3.4 資源上限 → cgroup translation (§5.A: the provider
// materializes the Binding via OCI linux.resources; runsc lands it on the
// cgroupfs driver).
//
// `pids` is DELIBERATELY ABSENT and must stay so. It is not "deferred" — it is
// permanently rejected: measured, a pids cap limits HOST tasks (~2.3 per guest
// process under systrap), and setting it low makes the sentry panic outright
// ("failed to get clone") and the sandbox vanish. That makes it an availability
// landmine rather than a resource cap, and §3.4's limits are only CPU/RAM/disk.
type ociResources struct {
	CPU    *ociCPU    `json:"cpu,omitempty"`
	Memory *ociMemory `json:"memory,omitempty"`
}

// ociCPU is the cpu-controller share (→ cgroup v2 cpu.max = "quota period").
type ociCPU struct {
	Quota  int64  `json:"quota,omitempty"`
	Period uint64 `json:"period,omitempty"`
}

// ociMemory is the memcg ceiling. Swap MUST be set equal to Limit, which is the
// OCI spelling of cgroup v2 `memory.swap.max=0` — a measured hard requirement,
// not a tuning choice: with a limit and no swap bound, the cgroup simply pages
// out into ~13 GiB of zram (a 192 MiB cap absorbed 1 GiB of allocation with an
// oom count of 0), so the "cap" enforces nothing.
//
// `memory.high` is deliberately never set: with swap.max=0 and unreclaimable
// anon pages, throttling degenerates into a hang (measured: 1282 throttle events
// and a guest stalled past 45 s) instead of a clean termination.
type ociMemory struct {
	Limit int64 `json:"limit,omitempty"`
	Swap  int64 `json:"swap,omitempty"`
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

// cpuPeriodMicros is the cgroup v2 cpu.max period we pin (100 ms, the kernel
// default). CPUMillis is expressed against it: quota = period * millis / 1000, so
// 500 millis → "50000 100000". Pinned rather than inherited so the quota math is
// stable across kernels.
const cpuPeriodMicros = 100_000

// buildOCISpec returns the config.json for a merged rootfs + adopted netns.
// entrypoint is sandboxd (init mode → PID 1, §5.E.1). netnsPath is the
// engine-pre-built netns runsc adopts (§5.A: --network=sandbox only adopts).
//
// cgroupsPath is the LEAF path relative to the cgroup v2 root, synthesized by
// local_runsc from the cid. The ownership split it encodes is a MEASURED hard
// constraint, and getting it backwards makes the cap silently not apply (§5.A):
// runsc creates only the FINAL segment and only if the parent already exists,
// and it writes limits ONLY to a leaf it created itself. If the engine
// pre-creates the full path, runsc writes memory.swap.max=0 but leaves
// memory.max=max — the RAM cap does not bind AND the launch still succeeds. So
// the engine mkdirs the PARENT (+ delegates subtree_control) and leaves the leaf
// to runsc; the caller then asserts the cap actually landed.
//
// limits zero on every dimension → no resources block, no cgroupsPath, no leaf.
func buildOCISpec(mergedPath, netnsPath, cgroupsPath string, limits ResourceLimits, memOverhead int64) *ociSpec {
	var res *ociResources
	if limits.CPUMillis > 0 || limits.MemoryByte > 0 {
		res = &ociResources{}
		if limits.CPUMillis > 0 {
			res.CPU = &ociCPU{
				Quota:  cpuPeriodMicros * limits.CPUMillis / 1000,
				Period: cpuPeriodMicros,
			}
		}
		if limits.MemoryByte > 0 {
			// The RAM "one knob / two values" split (§5.A / §3.4). The HOST ceiling is
			// N + overhead, not N: runsc derives the guest-visible MemTotal from the
			// OCI limit at boot, so setting both to N tells the guest it has N while
			// the whole sandbox — sentry and gofer included — is terminated somewhere
			// BELOW N (measured: killed at ~0.88N). The guest must be told N and the
			// host must allow N plus the substrate's own footprint.
			//
			// memOverhead is local_runsc's IMPLEMENTATION CONSTANT, not a contract
			// member: a remote provider's overhead is a different thing entirely (VM
			// balloon, scheduler memory request), which is exactly why ResourceLimits
			// says only N (§5.A).
			//
			// Subtlety worth stating, since it is what makes this work: the guest is
			// told N because runsc reads the limit from the OCI spec / its cgroup at
			// BOOT. So the spec carries N (guest-visible truth) and the caller raises
			// memory.max to N+overhead AFTER launch (host ceiling) — measured: the
			// post-launch bump does not move the guest's MemTotal.
			res.Memory = &ociMemory{
				Limit: limits.MemoryByte,
				// Swap == Limit ⇒ memory.swap.max=0: the cap is a hard ceiling rather
				// than a doorway into zram (see ociMemory).
				Swap: limits.MemoryByte,
			}
		}
	}
	if res == nil {
		cgroupsPath = "" // no limits → no leaf, no cgroup namespace footprint
	}
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
			User: ociUser{UID: 0, GID: 0},
			Args: []string{"/sbin/sandboxd", "init"},
			Env:  []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"},
			Cwd:  "/",
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
			CgroupsPath: cgroupsPath,
			Resources:   res,
		},
	}
}
