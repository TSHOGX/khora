package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// M6 acceptance 10 — STRUCTURAL assertion: no host primitive appears in the
// §5.A / §5.B seam signatures. No cgroup path, no host filesystem path, no memcg
// concept (§5.A ❌ list / §1.3 seam).
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
	// Substrings that betray a host primitive crossing a seam. Deliberately
	// includes the storage/memcg vocabulary as well as cgroup: the §5.B seam takes
	// a plain byte count, and the §4.3 death cause crosses as an abstract enum
	// (never an errno or a memory.events read), so neither may name a mechanism.
	banned := []string{
		"cgroup", "memcg", "oom",
		"upperdir", "workdir", "hostpath", "abspath",
		"subvolume", "qgroup", "squota", "errno",
	}
	// MergedPath is the ONE allowed host-shaped name and only as a RootfsSource
	// field: §5.A explicitly permits an already-merged rootfs handle as an input
	// (what is banned is the Overlay's INTERNAL upper/work paths). It is also
	// exempted by being a struct field, not a method parameter — the check below
	// only reads interface method signatures.

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
				for _, b := range banned {
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
			t.Errorf("ResourceLimits has unexpected field %q — capacity belongs to the FS provider (§3.4) and overhead is provider-internal (§5.A)", f)
		}
		delete(want, f)
	}
	for f := range want {
		t.Errorf("ResourceLimits is missing the policy field %q", f)
	}
}
