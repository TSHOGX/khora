package runtime

// Minimal OCI runtime-spec (config.json) builder. We emit only the fields runsc
// needs, not the full runtime-spec — pulling the whole
// opencontainers/runtime-spec module in for a handful of fields would be weight
// the engine vocabulary boundary rejects.

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
	// CgroupsPath MUST be set explicitly whenever we emit resources: omit it and
	// runsc builds its leaf at the cgroup v2 ROOT, polluting the host namespace.
	CgroupsPath string `json:"cgroupsPath,omitempty"`
	// Resources carries the resource admission rate + RAM Binding translated into OCI form.
	// omitempty: a sandbox with no limits emits no resources block at all, so
	// runsc creates no cgroup leaf and leaves no host cgroup footprint.
	Resources *ociResources `json:"resources,omitempty"`
}

// ociResources is the resource admission resource-ceiling → cgroup translation (the runtime provider contract: the provider
// materializes the Binding via OCI linux.resources; runsc lands it on the
// cgroupfs driver).
//
// `pids` is DELIBERATELY ABSENT and must stay so. It is not "deferred" — it is
// permanently rejected: measured, a pids cap limits HOST tasks (~2.3 per guest
// process under systrap), and setting it low makes the sentry panic outright
// ("failed to get clone") and the sandbox vanish. That makes it an availability
// landmine rather than a resource cap, and resource admission's limits are only CPU/RAM/disk.
type ociResources struct {
	CPU    *ociCPU    `json:"cpu,omitempty"`
	Memory *ociMemory `json:"memory,omitempty"`
}

// ociCPU carries both halves of the CPU axis. Quota/Period become cgroup v2
// `cpu.max` (the CEILING — "at most this much"), Shares becomes `cpu.weight`
// (the SHARE — "this much relative to whoever else is runnable").
//
// Both are required because they answer different questions. resource admission calls the CPU axis
// fair allocation, and a ceiling alone does not deliver it: cgroup's default weight is
// identical for every leaf, so on a saturated node a 4000-millis sandbox and a
// 250-millis one get EQUAL time — each capped correctly, sharing nothing like the ratio
// they were sold (measured). Weight is what makes the oversubscription resource admission permits (no
// CPU admission gate, since overrun only slows the guest) degrade proportionally rather
// than uniformly. It bites only when the node is saturated and both are runnable.
type ociCPU struct {
	Quota  int64  `json:"quota,omitempty"`
	Period uint64 `json:"period,omitempty"`
	Shares uint64 `json:"shares,omitempty"`
}

// cpuSharesPerCPU is the conventional OCI shares value for one full CPU. The
// runtime rescales shares into cgroup v2's 1..10000 `cpu.weight` range, so what
// matters here is only the RATIO between sandboxes, not the absolute number.
const cpuSharesPerCPU = 1024

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

// caps is the guest capability set. It is deliberately BROAD, not minimal: the
// guest runs as root by design and guest capabilities are not a host-isolation
// pillar (the guest identity model — sentry virtualizes them all, and sentry itself is unprivileged
// on the host, the isolation model). So this list is guest-internal hygiene only: enough for
// sandboxd to bind its netns TCP port, exec processes, and let `apt`/`pip`
// chown/chmod as they install (the engine vocabulary boundary root-capable sandbox).
//
// CAP_NET_RAW is DELIBERATELY ABSENT, and its absence is the interesting entry.
// Requesting it here would not grant it: measured, the guest's CapEff has the
// NET_RAW bit CLEAR, because runsc's own `--net-raw=false` default strips
// raw/AF_PACKET sockets regardless of what the OCI spec asks for. Asking
// for a capability that can never be granted is a FAKE INVARIANT — a reader takes
// it for a deliberate grant — so it is absent here and the real guarantee is
// PINNED on the runsc side instead (`--net-raw=false` in runscGlobalFlags, which
// carries why it is pinned rather than inherited).
//
// CAP_NET_ADMIN STAYS. It is guest-internal network work (routes, interfaces),
// and the guest identity model is explicit that guest caps are not an isolation pillar.
var caps = []string{
	"CAP_NET_BIND_SERVICE", "CAP_NET_ADMIN",
	"CAP_SYS_ADMIN", "CAP_CHOWN", "CAP_DAC_OVERRIDE",
	"CAP_KILL", "CAP_SETUID", "CAP_SETGID", "CAP_FOWNER",
}

// cpuPeriodMicros is the cgroup v2 cpu.max period we pin (100 ms, the kernel
// default). CPUMillis is expressed against it: quota = period * millis / 1000, so
// 500 millis → "50000 100000". Pinned rather than inherited so the quota math is
// stable across kernels.
const cpuPeriodMicros = 100_000

// buildOCISpec returns the config.json for a merged rootfs + adopted netns.
// entrypoint is sandboxd (init mode → PID 1, the guest startup topology). netnsPath is the
// engine-pre-built netns runsc adopts (the runtime provider contract: --network=sandbox only adopts).
//
// cgroupsPath is the LEAF path relative to the cgroup v2 root, synthesized by
// local_runsc from the cid. The ownership split it encodes is a MEASURED hard
// constraint, and getting it backwards makes the cap silently not apply (the runtime provider contract):
// runsc creates only the FINAL segment and only if the parent already exists,
// and it writes limits ONLY to a leaf it created itself. If the engine
// pre-creates the full path, runsc writes memory.swap.max=0 but leaves
// memory.max=max — the RAM cap does not bind AND the launch still succeeds. So
// the engine mkdirs the PARENT (+ delegates subtree_control) and leaves the leaf
// to runsc; the caller then asserts the cap actually landed.
//
// limits zero on every dimension → no resources block, no cgroupsPath, no leaf.
func buildOCISpec(mergedPath, netnsPath, cgroupsPath string, limits ResourceLimits, memOverhead int64, mounts []MountSource) *ociSpec {
	var res *ociResources
	if limits.CPUMillis > 0 || limits.MemoryByte > 0 {
		res = &ociResources{}
		if limits.CPUMillis > 0 {
			// Ceiling and share are DERIVED FROM THE SAME CPUMillis, so this stays
			// one knob in the contract (the extension boundary: no new field, no seam change) — the
			// caller states "how much CPU", and the provider decides that this means
			// both a cap and a proportional share. Shares floors at 2, the minimum
			// the OCI runtime will map into a valid cpu.weight.
			shares := uint64(cpuSharesPerCPU * limits.CPUMillis / 1000)
			if shares < 2 {
				shares = 2
			}
			res.CPU = &ociCPU{
				Quota:  cpuPeriodMicros * limits.CPUMillis / 1000,
				Period: cpuPeriodMicros,
				Shares: shares,
			}
		}
		if limits.MemoryByte > 0 {
			// The RAM "one knob / two values" split (the runtime provider contract / resource admission). The HOST ceiling is
			// N + overhead, not N: runsc derives the guest-visible MemTotal from the
			// OCI limit at boot, so setting both to N tells the guest it has N while
			// the whole sandbox — sentry and gofer included — is terminated somewhere
			// BELOW N (measured: killed at ~0.88N).
			//
			// So the spec carries N — runsc reads it at BOOT to derive MemTotal — and
			// bindLimits raises the host ceiling to N+overhead after launch. memOverhead
			// is local_runsc's own constant, never a contract member (memOverheadBytes).
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
	// The pseudo-filesystems the guest always gets, then the Volume subsystem Volume binds.
	//
	// VOLUMES GO LAST, deliberately: appending after the fixed set means a volume
	// destination can never displace /proc, /dev, /sys or /tmp — with runsc taking
	// mounts in spec order, an earlier volume at one of those paths would shadow the
	// pseudo-fs the guest needs to boot. Ordering here is a structural guarantee, so it
	// costs nothing and removes a whole class of destination.
	ociMounts := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "nodev", "mode=1777"}},
	}
	for _, mt := range mounts {
		// A plain bind, which is exactly the shape measured to work: with
		// --overlay2=none --file-access=shared, runsc proxies an extra rw host directory
		// through the gofer into the guest bidirectionally (the local backend relies on this
		// bind contract), and a host FUSE mount point proxies identically
		// (the future remote arm's foundation). Nothing here distinguishes the two.
		opts := []string{"rbind", "rprivate"}
		if mt.ReadOnly {
			// The ro half of shared-ro: past this, the guest's write gets a clean EROFS
			// rather than silently succeeding. See MountSource.ReadOnly for why the
			// direction of this bit is load-bearing.
			opts = append(opts, "ro")
		}
		ociMounts = append(ociMounts, ociMount{
			Destination: mt.Dest,
			Type:        "bind",
			Source:      mt.SourcePath,
			Options:     opts,
		})
	}

	return &ociSpec{
		OCIVersion: "1.0.0",
		Process: ociProcess{
			Terminal: false,
			// Guest root (uid/gid 0). This is the engine default, not a stub: guest
			// identity is not a host-isolation pillar (the guest identity model — sentry virtualizes guest
			// uid 0, sentry is unprivileged on the host), so guest-root costs nothing
			// on the isolation axis and is the most direct expression of the engine vocabulary boundary's
			// "root-capable agent sandbox". PID 1 is sandboxd init; it stays root so
			// it can hold CAP_NET_* for the netns transport port (the runtime provider contract).
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
		Mounts:   ociMounts,
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
