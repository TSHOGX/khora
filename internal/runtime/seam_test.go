package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
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
	"cgroup", "memcg", "oom",
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
