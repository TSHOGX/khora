package volume

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// bannedTokens is the runtime provider contract host-primitive list KEPT VERBATIM, plus the four
// storage-form spellings the Volume subsystem adds (the volume backend seam). Keeping the inherited list
// unedited is deliberate: the Volume subsystem seam is under the same rule as the runtime provider contract/the Rootfs subsystem, and a
// trimmed copy would quietly narrow it.
//
// The four additions each name a MECHANISM a volume backend might be built on —
// and a mechanism name on the contract surface is what binds the seam to one
// implementation. `reflink`, `fuse`, `loopdev`, and the storage-unit words name
// possible backend mechanisms; `mountopt` means "let the caller pass mount flags",
// which is the most tempting of
// the four because it looks like configuration rather than a leak.
//
// One list, both halves of the guard (signatures and struct fields) — a token
// banned from a parameter must be equally banned from a field, or the cheaper
// spelling becomes the way in.
var bannedTokens = []string{
	// inherited from the runtime provider contract, verbatim
	"cgroup", "memcg", "oom", "netns",
	"upperdir", "workdir", "hostpath", "abspath",
	"subvolume", "qgroup", "squota", "errno",
	"major", "minor", "majmin", "devno",
	// the Volume subsystem additions (the volume backend seam)
	"reflink", "fuse", "loopdev", "mountopt",
}

// fieldExemptions are the exported seam fields permitted to carry a host-shaped
// name, keyed "Type.Field" WITH THE REASON. The reason is the point: an exemption
// without one is a bare allowlist, and the next leak arrives through the same door
// claiming the same precedent.
var fieldExemptions = map[string]string{
	// The Volume subsystem twin of rootfs.Merged.MergedPath, and the ONLY exemption this seam
	// has. the Volume subsystem's whole job is to yield one host mount point for runsc to bind, so
	// the handle must carry it; what makes it a handle rather than a leak is that
	// nothing above the Volume subsystem interprets it (only local_runsc consumes it, exactly as with
	// MergedPath), volumes are addressed by ID everywhere else (the ID-addressed Overlay model / the API error contract), and
	// the strictly-narrower OpenRoot accessor carries NO path at all — so this is the
	// seam's single, bounded concession rather than its general posture.
	"Materialized.SourcePath": "the Volume subsystem yields one host mount point as an opaque bind source (rootfs.Merged.MergedPath's precedent); consumed only by local_runsc, and OpenRoot deliberately exposes no path at all",
}

// seamIfaces / seamStructs name what the guards scan. Declared here rather than
// inline so the reverse-injection tests below drive the SAME scanners over
// synthetic source — a guard whose failure path is never exercised is a guard
// nobody has checked.
var (
	seamIfaces  = []string{"Backend", "Root"}
	seamStructs = []string{"Materialized", "Capabilities", "FileInfo"}
)

// TestSeamCarriesNoHostPrimitives is the Volume subsystem twin of the runtime provider contract/the Rootfs subsystem guard: no host
// primitive appears in the volume.Backend contract.
//
// Two halves, because a host primitive can cross a seam two ways and both cost the
// same abstraction:
//
//  1. interface METHOD SIGNATURES — parameter names, parameter types, results.
//  2. the exported FIELDS of the data types that ride those signatures.
//
// Half 2 is not redundant: Materialized renders as the bare identifier
// "Materialized" in a signature, so a `SubvolumePath string` field on it would be
// invisible to a parameter-only scan — and a struct field is the MORE likely
// spelling, since that is where wiring a storage detail through naturally lands.
//
// It reads the DECLARED types out of the source AST, so it constrains the CONTRACT
// rather than any implementation's internals. LocalBtrfs legitimately handles
// storage units and quota rows — inside its own body, which is where they belong.
func TestSeamCarriesNoHostPrimitives(t *testing.T) {
	src, err := os.ReadFile("volume.go")
	if err != nil {
		t.Fatalf("read volume.go: %v", err)
	}
	violations, err := scanSeam(string(src))
	if err != nil {
		t.Fatalf("scan volume.go: %v", err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// TestCapabilitiesMembersAreBool pins the OTHER half of the Volume storage seam rule, and it
// is not stylistic. A `map[string]string` feature table would carry MECHANISM NAMES
// ACROSS THE SEAM AS VALUES — and a value is invisible to every guard above, which
// scans identifiers. So the shape itself has to be constrained: bools only, which
// can express "can it" and nothing else.
//
// It also pins that every member has a CONSUMER (the extension boundary forbids speculative
// reservation), and that the struct is not empty — an empty Capabilities would pass
// a members-are-bool check vacuously.
func TestCapabilitiesMembersAreBool(t *testing.T) {
	src, err := os.ReadFile("volume.go")
	if err != nil {
		t.Fatalf("read volume.go: %v", err)
	}
	violations, err := scanCapabilities(string(src))
	if err != nil {
		t.Fatalf("scan volume.go: %v", err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// TestGuardsCatchInjectedLeaks is the REVERSE-INJECTION verification the three
// guards need to be worth anything.
//
// A structural guard that has only ever run against clean source has never
// demonstrated that it FAILS — and the failure path is the entire product. Each
// case below is a leak someone would plausibly write while wiring a storage detail
// through, fed to the real scanner:
//
//   - a host-shaped FIELD on the handle (the cheapest and most likely leak)
//   - a host-shaped PARAMETER on a seam method
//   - a map-typed capability table, the leak the token scanners CANNOT see
//
// If a scanner is ever weakened — a token dropped, a half removed, an exemption
// widened to a prefix — the corresponding case here stops failing and this test
// says so.
func TestGuardsCatchInjectedLeaks(t *testing.T) {
	// A minimal but structurally faithful stand-in for volume.go: same interface and
	// struct names, so the scanners find their targets.
	const clean = `package volume

import "context"

type Capabilities struct {
	Snapshot bool
	Clone    bool
	Quota    bool
 Usage bool
}

type Materialized struct {
	Volume     string
	SourcePath string
	handle     any
}

type FileInfo struct {
	Name string
}

type Root interface {
	Stat(name string) (FileInfo, error)
	Close() error
}

type Backend interface {
	Name() string
	Capabilities() Capabilities
	Materialize(ctx context.Context, volumeID string) (*Materialized, error)
	OpenRoot(ctx context.Context, volumeID string) (Root, error)
}
`
	// Sanity: the stand-in must itself be clean, or every case below "passes" for
	// the wrong reason.
	if v, err := scanSeam(clean); err != nil {
		t.Fatalf("scan the clean stand-in: %v", err)
	} else if len(v) != 0 {
		t.Fatalf("the clean stand-in already reports violations, so the injections below prove nothing: %v", v)
	}
	if v, err := scanCapabilities(clean); err != nil {
		t.Fatalf("scan the clean stand-in: %v", err)
	} else if len(v) != 0 {
		t.Fatalf("the clean stand-in already reports Capabilities violations: %v", v)
	}

	for _, tc := range []struct {
		name string
		// old/new is a single textual substitution applied to the clean stand-in.
		old, new string
		scan     func(string) ([]string, error)
		wantIn   string
	}{{
		// Half 2: a storage-unit path smuggled onto the handle as a field. This is the
		// leak the parameter-only scan would miss entirely, since Materialized renders
		// as a bare identifier in every signature.
		name:   "SubvolumePath field on the handle",
		old:    "\tSourcePath string",
		new:    "\tSourcePath string\n\tSubvolumePath string",
		scan:   scanSeam,
		wantIn: "subvolume",
	}, {
		// Half 1: a remote arm's mechanism as a parameter name. `fuse` is banned
		// precisely so the seam cannot grow a shape only one arm can satisfy.
		name:   "FuseSocket parameter on a seam method",
		old:    "Materialize(ctx context.Context, volumeID string) (*Materialized, error)",
		new:    "Materialize(ctx context.Context, volumeID string, fuseSocket string) (*Materialized, error)",
		scan:   scanSeam,
		wantIn: "fuse",
	}, {
		// The bool guard: a feature MAP. Note the injected member's NAME ("Features")
		// is entirely innocent — the leak is in the values it would carry, which is
		// exactly why a token scan cannot catch it and a shape guard must.
		name:   "map-typed capability table",
		old:    "\tQuota    bool",
		new:    "\tQuota    bool\n\tFeatures map[string]string",
		scan:   scanCapabilities,
		wantIn: "not bool",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			injected := strings.Replace(clean, tc.old, tc.new, 1)
			if injected == clean {
				t.Fatalf("injection did not apply — this test is stale against the stand-in")
			}
			violations, err := tc.scan(injected)
			if err != nil {
				t.Fatalf("scan injected source: %v", err)
			}
			if len(violations) == 0 {
				t.Fatalf("the guard did NOT catch the injected leak %q — it has been weakened, and a guard that cannot fail is not coverage", tc.name)
			}
			joined := strings.ToLower(strings.Join(violations, "\n"))
			if !strings.Contains(joined, tc.wantIn) {
				t.Errorf("the guard caught something, but not the injected leak (want a report mentioning %q):\n%s", tc.wantIn, strings.Join(violations, "\n"))
			}
		})
	}
}

// scanSeam returns one message per host primitive found in the seam declared by
// src. Split out from the test so the reverse-injection cases above can drive the
// SAME code over synthetic source.
func scanSeam(src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "volume.go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []string

	// Half 1 — the interfaces on this seam. Root is included deliberately: it is a
	// contract member (Backend.OpenRoot returns it), so its methods are as much part
	// of the seam as Backend's.
	for _, iface := range seamIfaces {
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != iface {
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
				// Parameter NAMES matter as much as types: `subvolumePath string` leaks
				// the concept even though its type is a plain string.
				var sig strings.Builder
				writeFields(&sig, fset, fn.Params)
				writeFields(&sig, fset, fn.Results)
				got := strings.ToLower(sig.String())
				for _, b := range bannedTokens {
					if strings.Contains(got, b) {
						out = append(out, fmt.Sprintf("%s.%s signature leaks host primitive %q (the runtime provider contract list / the volume backend seam): %s",
							iface, name, b, sig.String()))
					}
				}
			}
			return false
		})
		if !found {
			return nil, fmt.Errorf("interface %s not found (guard is stale — renamed?)", iface)
		}
	}

	// Half 2 — the data types that cross those signatures.
	want := make(map[string]bool, len(seamStructs))
	for _, s := range seamStructs {
		want[s] = true
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
			// Only EXPORTED fields are part of the contract, so that a backend can keep
			// private state (a handle, a client) without it becoming contract — flagging
			// an unexported field would punish the pattern that keeps the seam clean.
			// `runtime.Placement.handle` is that pattern with live callers.
			names := exportedFieldNames(fld)
			if len(names) == 0 {
				continue
			}
			subject := strings.Join(names, " ") + " " + typeString(fset, fld.Type)
			for _, fieldName := range names {
				if _, exempt := fieldExemptions[ts.Name.Name+"."+fieldName]; exempt {
					continue
				}
				got := strings.ToLower(subject)
				for _, b := range bannedTokens {
					if strings.Contains(got, b) {
						out = append(out, fmt.Sprintf("%s.%s leaks host primitive %q across the seam (the runtime provider contract list / the volume backend seam): %s",
							ts.Name.Name, fieldName, b, strings.TrimSpace(subject)))
					}
				}
			}
		}
		return false
	})
	for name := range want {
		return nil, fmt.Errorf("struct %s not found (guard is stale — renamed, or no longer a struct?)", name)
	}
	return out, nil
}

// scanCapabilities returns one message per Capabilities member that is not a bool
// or has no consumer, plus one if the struct is missing or empty.
func scanCapabilities(src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "volume.go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []string
	var fields []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Capabilities" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			typ := typeString(fset, fld.Type)
			for _, n := range fld.Names {
				fields = append(fields, n.Name)
				if typ != "bool" {
					out = append(out, fmt.Sprintf("Capabilities.%s is %s, not bool — a non-bool capability member can smuggle a MECHANISM NAME across the seam as a VALUE, where no identifier-scanning guard can see it (the volume backend seam)",
						n.Name, typ))
				}
			}
		}
		return false
	})
	if len(fields) == 0 {
		return nil, fmt.Errorf("Capabilities not found or has no members (guard is stale — an empty struct would pass vacuously)")
	}
	// Members must have a CONSUMER (the extension boundary forbids speculative reservation). This is
	// the settled set; adding one means naming its consumer here too.
	want := map[string]bool{"Snapshot": true, "Clone": true, "Usage": true, "Quota": true}
	for _, fname := range fields {
		if !want[fname] {
			out = append(out, fmt.Sprintf("Capabilities has unexpected member %q — every member needs a live consumer (the extension boundary): Snapshot/Clone gate the control-plane API endpoints, Quota gates resource admission's refuse-rather-than-run-uncapped", fname))
		}
		delete(want, fname)
	}
	for fname := range want {
		out = append(out, fmt.Sprintf("Capabilities is missing member %q", fname))
	}
	return out, nil
}

// --- AST helpers, same shape as internal/runtime/seam_test.go ----------------

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
