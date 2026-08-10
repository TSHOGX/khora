package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"engine/internal/ruleset"
)

// bannedTokens are substrings that betray a host primitive crossing a seam.
// Deliberately includes the storage/memcg vocabulary as well as cgroup: the §5.B
// seam takes a plain byte count, and the §4.3 death cause crosses as an abstract
// enum (never an errno or a memory.events read), so neither may name a mechanism.
//
// One list, shared by BOTH halves of the guard below (signatures and data-type
// fields) — a token banned from a parameter must be equally banned from a field,
// or the cheaper of the two spellings becomes the way in.
//
// The block-device vocabulary is banned for a reason worth stating, because the
// axis it guards is not built yet. When the disk-IO RATE cap lands (§3.4's other
// rate-axis half), `io.max` will need a WHOLE-DISK MAJ:MIN, and the settled
// answer is that the provider DERIVES it from its own state dir rather than
// receiving it (a device number is irreducibly host-local, so a remote provider
// could not produce one — §5.A). Matching is by name SUBSTRING, and no token
// above would have stopped a field spelled `MajMin` or `MajorNum`: without these
// four, the decision's half-life is however long until the next person wires
// that cap through a struct field, which is exactly the cheap-at-the-time leak
// this test exists to catch. They ban the SPELLINGS of a device number, not
// every synonym for one — a field named e.g. `DiskDevice` still gets through, so
// this backstops the §5.A rule rather than replacing the need to know it.
var bannedTokens = []string{
	"cgroup", "memcg", "oom", "netns",
	"upperdir", "workdir", "hostpath", "abspath",
	"subvolume", "qgroup", "squota", "errno",
	"major", "minor", "majmin", "devno",
}

// fieldExemptions are the exported seam fields permitted to carry a host-shaped
// name, keyed "Type.Field" with the reason it is legitimate. The reason is the
// point: an exemption without one is a bare allowlist, and the next leak arrives
// through the same door claiming the same precedent.
var fieldExemptions = map[string]string{
	// §5.A explicitly permits an ALREADY-MERGED rootfs handle as a provider input.
	// The provider is told "run this rootfs" and learns nothing about how it was
	// assembled; what §5.A bans is the Overlay's INTERNAL upper/work paths, which
	// would expose the overlayfs mechanism and make the seam unimplementable by a
	// provider that assembles rootfs some other way. MergedPath is populated and
	// consumed only by local_runsc and never enters a cross-provider decision
	// (RootfsSource's own doc comment, and open-decisions §5.A for the remote form).
	"RootfsSource.MergedPath": "§5.A permits a merged-rootfs handle as provider input; only upper/work are banned",
	// The §5.B side of that same handoff: Merged is what the Rootfs subsystem hands
	// across as an opaque RootfsSource. It is the ONE host-shaped name the storage
	// seam exposes — upper/work deliberately do not appear on Overlay at all (the
	// provider recomputes them from the ID, §3.1.1: addressed by ID internally too), which is what
	// keeps this single field a handle rather than a leak of the mechanism.
	"Merged.MergedPath": "§5.B hands the merged rootfs across as an opaque handle; upper/work stay private",
	// The §5.C twin of the two above, and exempted for the identical reason: §5.C's
	// seam yields exactly ONE thing — a host mount point — which this provider then
	// writes into an OCI bind. It is populated and consumed only by local_runsc, never
	// interpreted above §5.C, and volumes are addressed by ID everywhere else (§6.2).
	// The exemption is EXPLICIT rather than incidental: without an entry here the field
	// would pass only because no banned token happens to appear in "SourcePath", which
	// is a coincidence and not a decision.
	"MountSource.SourcePath": "§5.C yields one host mount point as an opaque bind source (RootfsSource.MergedPath's precedent); consumed only by local_runsc, and the volume's ID/access mode deliberately do not cross",
}

// STRUCTURAL assertion: no host primitive appears in the
// §5.A / §5.B seam. No cgroup path, no host filesystem path, no memcg concept
// (§5.A ❌ list / §1.3 seam).
//
// Two halves, because a host primitive can cross a seam two ways and both cost
// the same abstraction:
//
//  1. interface METHOD SIGNATURES — parameter names, parameter types, results.
//  2. the exported FIELDS of the data types that ride those signatures.
//
// Half 2 exists because half 1 alone is weaker than the claim: LaunchSpec renders
// as the bare identifier "LaunchSpec" in a signature, so a `CgroupPath string`
// field on it would be invisible to a parameter-only scan — and a struct field is
// the MORE likely spelling, since wiring a cgroup cap through a spec struct is
// exactly where the temptation lands.
//
// This is a test rather than a review habit because the pressure to leak is
// constant and the leak is CHEAP at the moment you write it: while wiring a
// cgroup cap it is very natural to "just pass the leaf path down", and nothing
// else would fail. What that costs is the abstraction: a provider scheduling a
// remote isolation unit cannot produce a cgroup path or a host path at all, so a
// single leaked parameter makes the interface unimplementable by anything but a
// host-local provider — the exact binding §5.A exists to prevent.
//
// It reads the DECLARED interface types out of the source AST, so it constrains
// the contract itself rather than any implementation's internals. local_runsc
// legitimately handles cgroup paths, memcg files, and host paths — inside its own
// body, which is precisely where they belong.
func TestSeamCarriesNoHostPrimitives(t *testing.T) {
	for _, target := range []struct{ file, iface string }{
		{"provider.go", "Provider"},
		{"../rootfs/rootfs.go", "Provider"},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, target.file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", target.file, err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != target.iface {
				return true
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			found = true
			for _, m := range it.Methods.List {
				fn, ok := m.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				name := "<embedded>"
				if len(m.Names) > 0 {
					name = m.Names[0].Name
				}
				// Render the signature (parameter names + types, results) and scan it.
				// Parameter NAMES matter as much as types: `cgroupPath string` leaks the
				// concept even though its type is a plain string.
				var sig strings.Builder
				writeFields(&sig, fset, fn.Params)
				writeFields(&sig, fset, fn.Results)
				got := strings.ToLower(sig.String())
				for _, b := range bannedTokens {
					if strings.Contains(got, b) {
						t.Errorf("%s.%s signature leaks host primitive %q (§5.A ❌ list): %s",
							target.iface, name, b, sig.String())
					}
				}
			}
			return false
		})
		if !found {
			t.Fatalf("interface %s not found in %s (test is stale — did it get renamed?)",
				target.iface, target.file)
		}
	}

	// Half 2 — the DATA TYPES that cross those signatures as parameters or
	// returns. Every type listed here is reachable through the seam, so its
	// exported fields are as much a part of the contract as the parameter list.
	for _, target := range []struct {
		file  string
		types []string
	}{
		// §5.A: the contract 3-tuple (rootfs / steering / limits), the spec that
		// carries them, and the placement handle returned.
		// §5.A: the contract 3-tuple (rootfs / steering / limits), the spec that
		// carries them, the §5.C mount sources riding beside it, and the placement
		// handle returned. MountSource is listed because LaunchSpec renders as a bare
		// identifier in every signature, so a host primitive on it would otherwise be
		// invisible to both halves of this guard.
		{"provider.go", []string{
			"RootfsSource", "SteeringSpec", "ResourceLimits", "LaunchSpec", "MountSource", "Placement",
		}},
		// §5.B: the storage seam's two data types.
		{"../rootfs/rootfs.go", []string{"Overlay", "Merged"}},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, target.file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", target.file, err)
		}
		want := make(map[string]bool, len(target.types))
		for _, name := range target.types {
			want[name] = true
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !want[ts.Name.Name] {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			delete(want, ts.Name.Name)
			for _, fld := range st.Fields.List {
				// Only EXPORTED fields are part of the contract. An unexported field is
				// provider-private and correct by design: Placement.handle holds
				// local_runsc's container id + netns name precisely so callers cannot
				// reach them (they address by SandboxID, §3.3) — flagging it would
				// punish the pattern that keeps the seam clean.
				names := exportedFieldNames(fld)
				if len(names) == 0 {
					continue
				}
				// Field NAMES and TYPES both matter, for the same reason parameter names
				// do: `CgroupPath string` leaks the concept behind a plain string type,
				// and a field typed `cgroupHandle` leaks it behind an innocuous name.
				subject := strings.Join(names, " ") + " " + typeString(fset, fld.Type)
				for _, fieldName := range names {
					key := ts.Name.Name + "." + fieldName
					if why, exempt := fieldExemptions[key]; exempt {
						t.Logf("%s exempt: %s", key, why)
						continue
					}
					got := strings.ToLower(subject)
					for _, b := range bannedTokens {
						if strings.Contains(got, b) {
							t.Errorf("%s.%s leaks host primitive %q across the seam (§5.A ❌ list): %s",
								ts.Name.Name, fieldName, b, strings.TrimSpace(subject))
						}
					}
				}
			}
			return false
		})
		for name := range want {
			t.Fatalf("struct %s not found in %s (test is stale — renamed, or no longer a struct?)",
				name, target.file)
		}
	}
}

// exportedFieldNames returns the exported names a struct field declares. An
// EMBEDDED field has no names, so it is reported under its type name — embedding
// promotes the embedded type's fields into the outer type, which means it crosses
// the seam just as a named field does.
func exportedFieldNames(fld *ast.Field) []string {
	if len(fld.Names) == 0 {
		t := fld.Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		switch e := t.(type) {
		case *ast.Ident:
			if e.IsExported() {
				return []string{e.Name}
			}
		case *ast.SelectorExpr:
			if e.Sel.IsExported() {
				return []string{e.Sel.Name}
			}
		}
		return nil
	}
	var out []string
	for _, n := range fld.Names {
		if n.IsExported() {
			out = append(out, n.Name)
		}
	}
	return out
}

func writeFields(sb *strings.Builder, fset *token.FileSet, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, p := range fl.List {
		for _, n := range p.Names {
			sb.WriteString(n.Name + " ")
		}
		sb.WriteString(typeString(fset, p.Type) + " ")
	}
}

// typeString renders a type expression back to source-ish text.
func typeString(fset *token.FileSet, e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(fset, t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(fset, t.X)
	case *ast.ArrayType:
		return "[]" + typeString(fset, t.Elt)
	case *ast.Ellipsis:
		return "..." + typeString(fset, t.Elt)
	case *ast.MapType:
		return "map[" + typeString(fset, t.Key) + "]" + typeString(fset, t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		var sb strings.Builder
		sb.WriteString("func(")
		writeFields(&sb, fset, t.Params)
		sb.WriteString(")")
		writeFields(&sb, fset, t.Results)
		return sb.String()
	default:
		return "?"
	}
}

// The RAM two-values split is a PROVIDER-INTERNAL detail (§5.A / §3.4): the
// interface states only MemoryByte = N. This asserts the overhead constant is not
// reachable through the contract — ResourceLimits must carry exactly the numeric
// intent and nothing about how a provider pays for its own substrate.
func TestResourceLimitsCarriesOnlyPolicy(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "provider.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "ResourceLimits" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			for _, n := range fld.Names {
				fields = append(fields, n.Name)
			}
		}
		return false
	})
	if len(fields) == 0 {
		t.Fatal("ResourceLimits not found or has no fields (stale test?)")
	}
	// Exactly the two policy numbers. DiskByte must NOT be here: the capacity axis
	// is an FS quota owned by §5.B/§5.C, and putting it on the runtime provider
	// would weld two orthogonal axes onto one mechanism (§3.4).
	want := map[string]bool{"CPUMillis": true, "MemoryByte": true}
	for _, f := range fields {
		if !want[f] {
			t.Errorf("ResourceLimits has unexpected field %q — capacity belongs to the rootfs provider (§3.4) and overhead is provider-internal (§5.A)", f)
		}
		delete(want, f)
	}
	for f := range want {
		t.Errorf("ResourceLimits is missing the policy field %q", f)
	}
}

// pinnedFlag is one runsc global flag together with the § guarantee it carries.
// The `why` is the whole point of the table: a future edit that drops a key gets
// told what it just gave up, rather than seeing a bare string diff.
type pinnedFlag struct {
	flag string
	why  string
}

// pinnedRunscFlags is the §5.A pinned global flag set, by value. §5.A: local_runsc
// 钉死其依赖的整套 runsc flag、不继承默认 —— runsc's defaults drift across versions
// and §5.B's cross-runsc-version portability may not rest on "the default did not
// change".
var pinnedRunscFlags = []pinnedFlag{
	{"--platform=systrap", "§1.1 no-hypervisor. FAILS SILENTLY: systrap is also runsc's current default, so dropping this changes nothing observable TODAY and breaks the moment a runsc version moves the default (e.g. to KVM) — exactly the drift §5.A pins against"},
	{"--network=sandbox", "userspace netstack. host/hostinet lets the sentry use host sockets directly and violates §2 substrate pillar ① (REJECTED, not an alternative mode); it is also the precondition for §5.D steering (per-sandbox netns) and §5.E transport (netstack-terminated, dialed over the veth IP)"},
	{"--overlay2=none", "§5.B/§5.C own the persistent layer OUTSIDE runsc; the gofer only proxies host dirs. runsc's own overlay would put guest writes somewhere the engine does not manage, so Promote would freeze an incomplete tree"},
	{"--file-access=shared", "the other half of the same guarantee: the gofer proxies the host dir bidirectionally, which is what makes §5.C volume binds and the at-rest upper work at all (rootfs-overlay D0)"},
	{"--net-raw=false", "§5.D: the guest cannot forge L2 frames. runsc's own default already strips raw/AF_PACKET, so WITHOUT this pin the guarantee is an UNDECLARED default — and §5.D's future interface-keyed predicate floor would face a raw-socket-capable guest with no test firing. Paired with CAP_NET_RAW's absence from oci.go caps"},
	{"--systemd-cgroup=false", "cgroupfs driver, which the whole §5.A cgroup-ownership split assumes (engine mkdirs the parent, runsc creates the leaf). The systemd driver would place the leaf somewhere else entirely and the post-launch cap assertion would read the wrong path"},
}

// TestRunscGlobalFlagsArePinned asserts the EXACT §5.A pinned flag set, by value.
//
// This is a test rather than a review habit for the same reason the seam guard
// above is: two of these keys fail SILENTLY.
//
//   - `--platform=systrap` matches runsc's current default, so dropping it is
//     invisible until a runsc version moves that default.
//   - `--root` is LOAD-BEARING, not a path preference (§5.A): point it at the wrong
//     directory and `runsc state`/`list` return empty rather than erroring, which
//     makes §4.4-A reverse reconciliation read EVERY live placement as nonexistent
//     — and reclaim live sandboxes. A silently-empty enumeration is the worst shape
//     a failure can take here, since it looks exactly like "nothing is running".
//
// Both halves are asserted: every pinned key is PRESENT (with its § guarantee named
// in the failure), and no UNKNOWN flag has been added without a recorded reason.
func TestRunscGlobalFlagsArePinned(t *testing.T) {
	p := &LocalRunsc{RunscBin: "runsc", RunscRoot: "/var/run/engine-runsc", StateDir: "/var/lib/engine/state"}
	argv := p.runscGlobalFlags()

	if len(argv) == 0 || argv[0] != "runsc" {
		t.Fatalf("argv[0] = %q; want the runsc binary first (global flags follow it, subcommand after)", argv)
	}
	flags := argv[1:]

	// --root is asserted separately because it is VALUE-DERIVED (the configured
	// root), not a constant — and because it carries the heaviest consequence.
	wantRoot := "--root=" + p.RunscRoot
	if flags[0] != wantRoot {
		t.Errorf("first global flag = %q; want %q. §5.A: --root is the single addressing root for all runsc state — a wrong root makes state/list SILENTLY EMPTY, so §4.4-A reverse reconciliation reads every live placement as nonexistent and reclaims live sandboxes",
			flags[0], wantRoot)
	}

	got := make(map[string]bool, len(flags))
	for _, f := range flags {
		got[f] = true
	}
	for _, pf := range pinnedRunscFlags {
		if !got[pf.flag] {
			t.Errorf("pinned runsc flag %s is MISSING from runscGlobalFlags — it carries: %s", pf.flag, pf.why)
		}
	}

	// The reverse direction: an unrecorded flag. Not a style rule — an unpinned key
	// that someone adds by hand is one nobody has attached a guarantee to, and §5.A's
	// discipline is per-key ("哪些键承重、各对哪条 §保证").
	known := map[string]bool{wantRoot: true}
	for _, pf := range pinnedRunscFlags {
		known[pf.flag] = true
	}
	for _, f := range flags {
		if !known[f] {
			t.Errorf("runscGlobalFlags carries unrecorded flag %q — §5.A pins each key to ONE stated guarantee; add it to pinnedRunscFlags with the § it serves (or drop it)", f)
		}
	}

	// The flags must all be GLOBAL, i.e. emitted before any subcommand. runsc rejects
	// a global flag placed after the subcommand, so ordering here is load-bearing.
	for _, f := range flags {
		if !strings.HasPrefix(f, "--") {
			t.Errorf("runscGlobalFlags emits non-flag token %q — everything after the binary must be a global flag (the subcommand is appended by the callers)", f)
		}
	}
}

// CAP_NET_RAW must NOT be requested (§5.A / oci.go caps). Measured: the guest's
// CapEff has the bit clear regardless, because runsc's --net-raw=false strips
// raw/AF_PACKET sockets — so requesting it is a FAKE INVARIANT, a line that reads
// as a deliberate grant for something the guest can never hold. The real guarantee
// is the pinned flag above; this asserts the two halves stay consistent.
//
// CAP_NET_ADMIN is asserted PRESENT in the same test, because the tempting
// "tighten the caps" edit is to drop both — and §2.2 is explicit that guest caps
// are not an isolation pillar, so removing NET_ADMIN buys no isolation and costs
// legitimate in-guest network work.
func TestGuestCapsDoNotRequestNetRaw(t *testing.T) {
	var hasAdmin bool
	for _, c := range caps {
		if c == "CAP_NET_RAW" {
			t.Errorf("caps requests CAP_NET_RAW, which the guest provably never gets (measured: CapEff bit clear under --net-raw=false). Requesting an ungrantable cap is a fake invariant — the raw-socket guarantee belongs to the pinned --net-raw=false flag, not to this list (§5.A)")
		}
		if c == "CAP_NET_ADMIN" {
			hasAdmin = true
		}
	}
	if !hasAdmin {
		t.Error("caps dropped CAP_NET_ADMIN — §2.2: guest caps are explicitly NOT an isolation pillar (the sentry virtualizes them and is itself unprivileged on the host), so removing it buys no isolation and breaks legitimate in-guest network work")
	}
}

// A restarted allocator must not re-hand a survivor's IP to a NEW sandbox
// (§5.A / §4.4-A: 接管存活者时把其 IP 标进分配器 inUse，重启后的分配器不会把该 IP
// 再发给新沙箱).
//
// The failure mode is QUIET, which is why this is worth a test: the fresh process
// has an empty inUse map and a randomized ipSeq, so nothing stops it from handing a
// second sandbox the /30 a survivor is already using. Two sandboxes then share a
// subnet, and the transport endpoint for at least one of them resolves to the wrong
// guest — no error anywhere, just a sandbox answering as another.
//
// Simulated at the allocator level rather than through real runsc: what Adopt
// contributes on the alive path is exactly the markInUse(ip) call, and allocSubnet
// is the half that must honor it. Both are unit-testable, and the substrate suite
// covers the Adopt plumbing that produces the IP.
func TestRestartedAllocatorSkipsAnAdoptedSurvivorsIP(t *testing.T) {
	base := t.TempDir()
	p, err := NewLocalRunsc(base+"/root", base+"/state")
	if err != nil {
		t.Fatal(err)
	}

	// ipSeq is REWOUND explicitly at each step, and that is what makes this test
	// falsifiable rather than decorative. NewLocalRunsc randomizes the start, and the
	// sequence walks ~16000 distinct /30s, so "allocate a few and check none collide"
	// would pass whether or not the inUse check exists — the allocator simply would
	// not come back around to the survivor's block. Rewinding to the exact seq that
	// produced the survivor makes the NEXT call collide unless inUse stops it.
	rewind := func(seq uint32) {
		p.mu.Lock()
		p.ipSeq = seq
		p.mu.Unlock()
	}

	// The "survivor": an IP this restarted process learned from Adopt's alive path
	// (Adopt calls markInUse after rediscovering the endpoint). Taken from the
	// allocator itself so it is genuinely an address this instance hands out.
	rewind(0)
	survivorHost, survivorIP := p.allocSubnet()
	p.freeIP(survivorIP) // the fresh process never allocated it — it ADOPTED it...
	p.markInUse(survivorIP)

	// the /30 pairing endpoint rediscovery depends on: guest = host + 1
	// (rediscoverGuestIP reads the host veth address and adds one).
	if want := incLastOctet(t, survivorHost); survivorIP != want {
		t.Fatalf("allocSubnet gave host=%s guest=%s; guest must be host+1 or rediscoverGuestIP recovers the WRONG endpoint after a crash", survivorHost, survivorIP)
	}

	// ...and now the next allocation is pointed straight at it. Without the inUse
	// skip in allocSubnet this returns survivorIP: a duplicate /30, two sandboxes
	// sharing a subnet, and at least one transport endpoint resolving to the wrong
	// guest — with no error raised anywhere.
	rewind(0)
	_, fresh := p.allocSubnet()
	if fresh == survivorIP {
		t.Fatalf("allocSubnet handed a new sandbox %s, which an adopted survivor holds (§4.4-A: a restarted allocator must skip IPs Adopt marked in use)", fresh)
	}

	// Freeing the survivor (its placement was torn down) returns the IP to
	// circulation — otherwise a long-running process leaks its address space one
	// /30 at a time. Rewound to the same seq, so this is the same single call.
	p.freeIP(survivorIP)
	p.freeIP(fresh)
	rewind(0)
	if _, reissued := p.allocSubnet(); reissued != survivorIP {
		t.Errorf("after freeIP(%s) the allocator returned %s instead — a released IP must return to circulation", survivorIP, reissued)
	}
}

// Every /30 the allocator hands out must fall inside the CIDR §5.D's L0 floor
// names as its SIBLING item (§2.3 / network.md D5 ①④).
//
// The two facts share one leaf constant precisely so they cannot drift, and this
// asserts the allocator actually honors it — because the drift is SILENT and its
// consequence is the invariant §2.4 exists to deliver: a /30 allocated outside the
// floor's sibling item is a sandbox address the floor does not cover, i.e. a
// reachable sibling with every reachability test still passing (point-to-point
// veth does not isolate — measured, the host routes between two /30s and sandbox
// A completed a TCP handshake to sibling B).
func TestEveryAllocatedSubnetLiesInsideTheFloorsSiblingCIDR(t *testing.T) {
	base := t.TempDir()
	p, err := NewLocalRunsc(base+"/root", base+"/state")
	if err != nil {
		t.Fatal(err)
	}
	// Walk the whole allocator cycle rather than a handful: the wrap-around
	// arithmetic (`block % 16000`, third octet, base+1/base+2) is where an
	// out-of-range address would appear, and it appears only near the wrap.
	p.mu.Lock()
	p.ipSeq = 0
	p.mu.Unlock()
	for i := 0; i < 16000; i++ {
		hostIP, guestIP := p.allocSubnet()
		p.freeIP(guestIP)
		for _, ip := range []string{hostIP, guestIP} {
			a, perr := netip.ParseAddr(ip)
			if perr != nil {
				t.Fatalf("allocSubnet produced an unparseable address %q: %v", ip, perr)
			}
			if !ruleset.SandboxCIDR.Contains(a) {
				t.Fatalf("allocSubnet handed out %s at seq %d, outside the floor's sibling CIDR %s — "+
					"that address space is NOT covered by the L0 floor, so the sibling would be reachable "+
					"with every reachability assertion still passing (§2.3 / D5 ①④)", ip, i, ruleset.SandboxCIDR)
			}
		}
		// guest = host+1 within the /30 is what Adopt's endpoint rediscovery depends
		// on (it reads the host veth and adds one).
		if want := incLastOctet(t, hostIP); guestIP != want {
			t.Fatalf("seq %d: host=%s guest=%s; guest must be host+1", i, hostIP, guestIP)
		}
	}
}

func incLastOctet(t *testing.T, ip string) string {
	t.Helper()
	i := strings.LastIndex(ip, ".")
	if i < 0 {
		t.Fatalf("malformed IP %q", ip)
	}
	n, err := strconv.Atoi(ip[i+1:])
	if err != nil {
		t.Fatalf("malformed IP %q: %v", ip, err)
	}
	return ip[:i+1] + strconv.Itoa(n+1)
}
