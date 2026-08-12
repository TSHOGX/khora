package orchestrator

// The guards that make docs/spec/openapi.json a CONTRACT rather than a file
// somebody once wrote (ADR-0008 part 1).
//
// Three properties, and each one fails in a different way if left unchecked:
//
//  1. Not stale — the checked-in bytes are what the generator produces now.
//  2. Every listed operation is actually ROUTED. A document describing an endpoint
//     the engine does not serve is worse than no document: a consumer generates a
//     client against it and gets a 404 at runtime.
//  3. Every route the engine SERVES is listed. This is the direction a document
//     silently rots in — a new endpoint lands, nothing fails, and the published
//     contract quietly describes an older engine.

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found above the package directory")
	return ""
}

// TestOpenAPIDocIsNotStale: the checked-in document equals this build's output.
//
// Byte-for-byte, which is why OpenAPIDoc emits indented JSON with sorted keys —
// encoding/json sorts map keys, so the output is deterministic and a diff here means
// a real change rather than map iteration order.
func TestOpenAPIDocIsNotStale(t *testing.T) {
	want, err := OpenAPIDoc()
	if err != nil {
		t.Fatalf("OpenAPIDoc: %v", err)
	}
	path := filepath.Join(repoRoot(t), openAPIDocPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate: go run ./cmd/openapigen -out %s)", path, err, openAPIDocPath)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is STALE — the §6.1 surface changed and the published contract did not.\n"+
			"Regenerate in THIS change, not as a follow-up:\n"+
			"    go run ./cmd/openapigen -out %s\n"+
			"(checked-in %d bytes, generated %d bytes)",
			openAPIDocPath, openAPIDocPath, len(got), len(want))
	}
}

// TestOpenAPIDocIsWellFormedEnoughToGenerateFrom: the shape a consumer's generator
// relies on. Not a full 3.1 validation — that needs a validator the module does not
// carry — but the specific ways a hand-rolled emitter goes wrong.
func TestOpenAPIDocIsWellFormedEnoughToGenerateFrom(t *testing.T) {
	raw, err := OpenAPIDoc()
	if err != nil {
		t.Fatalf("OpenAPIDoc: %v", err)
	}
	var doc struct {
		OpenAPI string                           `json:"openapi"`
		Paths   map[string]map[string]any        `json:"paths"`
		Comps   struct{ Schemas map[string]any } `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}
	if doc.OpenAPI == "" {
		t.Error("no `openapi` version field — every generator dispatches on it")
	}
	if len(doc.Paths) == 0 {
		t.Fatal("no paths")
	}
	// An unhandled Go kind must never reach the wire as a described field: a consumer
	// would generate code from a schema the engine does not actually understand.
	if bytes.Contains(raw, []byte("x-unsupported-go-kind")) {
		t.Error("the document contains an unsupported Go kind — schemaFor met a type it " +
			"cannot render, so some field is described wrongly. Extend schemaFor rather " +
			"than letting it fall through.")
	}
	// Every $ref must resolve. A dangling one is the classic emitter bug and it breaks
	// generators at parse time rather than at runtime.
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if r, ok := x["$ref"].(string); ok {
				name := strings.TrimPrefix(r, "#/components/schemas/")
				if _, ok := doc.Comps.Schemas[name]; !ok {
					t.Errorf("dangling $ref %q", r)
				}
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	var anyDoc any
	_ = json.Unmarshal(raw, &anyDoc)
	walk(anyDoc)

	for _, name := range []string{"createReq", "sandboxResp", "eventsResp", "activityResp"} {
		if _, ok := doc.Comps.Schemas[name]; !ok {
			t.Errorf("schema %q missing — the document should describe it by reflection", name)
		}
	}
}

// TestEveryDocumentedOperationIsRouted drives each listed operation against a real
// server and asserts the router RECOGNIZES it: not 404, and not 405.
//
// It deliberately does not assert success. Most operations need state this test has
// no business building (a running sandbox with a live guest), and the property under
// test is "the engine serves this method at this path" — which the two negative codes
// answer exactly. A 400 or a 503 here is a PASS: the route ran and the handler
// objected to something else.
func TestEveryDocumentedOperationIsRouted(t *testing.T) {
	srvURL, _, closeFn := activityFixture(t, &activityFakeClient{barrierFakeClient: &barrierFakeClient{}})
	defer closeFn()
	srv := struct{ URL string }{srvURL}

	for _, op := range apiOps {
		t.Run(op.OpID, func(t *testing.T) {
			// A syntactically valid but absent id: the route must still be recognized,
			// which is what separates "no such route" from "no such object".
			path := strings.NewReplacer("{id}", "sbx_0000000000000000").Replace(op.Path)
			url := srv.URL + path
			if len(op.Query) > 0 {
				url += "?path=/tmp/x&after=0&limit=1"
			}
			req, err := http.NewRequest(op.Method, url, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", op.Method, path, err)
			}
			defer resp.Body.Close()
			// Discriminating "no such route" from "a handler ran and objected" cannot use
			// the content type: §6.2 error bodies go through writeErr → http.Error, so
			// EVERY error on this surface is text/plain, routed or not. What separates
			// them is the body: an unrouted path yields one of exactly two sentinels —
			// the mux's own "404 page not found" or the sub-switch's `default` arm — and
			// neither can be produced by a handler, which always reports what it could
			// not find. Any other 404 means the route ran and the object is simply
			// absent, which is all this fixture can offer for most of the table.
			body, _ := io.ReadAll(resp.Body)
			text := strings.TrimSpace(string(body))
			switch resp.StatusCode {
			case http.StatusNotFound:
				if text == "404 page not found" || text == "not found" {
					t.Errorf("%s %s → 404 %q: the document describes an endpoint the engine "+
						"does not route", op.Method, op.Path, text)
				}
			case http.StatusMethodNotAllowed:
				t.Errorf("%s %s → 405: the path is routed but this METHOD is not, so the "+
					"document describes an operation the engine does not serve", op.Method, op.Path)
			}
		})
	}
}

// TestEveryRoutedOperationIsDocumented is the direction that actually rots: a new
// endpoint lands, every test passes, and the published contract silently describes an
// older engine.
//
// It reads the ROUTER'S OWN SOURCE rather than probing, because there is no way to
// enumerate an http.ServeMux's patterns, and a prober cannot invent the paths it does
// not already know. The AST is the same technique edge_test.go uses for the
// sanitization guard — a structural fact checked structurally.
func TestEveryRoutedOperationIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, op := range apiOps {
		documented[op.Method+" "+op.Path] = true
	}

	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	// The sub-route switches spell their arms as `sub == "<name>" && r.Method ==
	// http.MethodX`, so a routed operation is discoverable as that pair. Collected per
	// collection prefix, which is the switch's own function.
	prefixes := map[string]string{
		"handleSandboxSub": "/v1/sandboxes/{id}",
		"handleVolumeSub":  "/v1/volumes/{id}",
	}
	var missing []string
	for _, f := range pkg {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			prefix, tracked := prefixes[fn.Name.Name]
			if !tracked {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				cc, ok := m.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, expr := range cc.List {
					sub, method, ok := subAndMethod(expr)
					if !ok || method == "" {
						// An arm with no method is the §6.2 catch-all that answers 405 for
						// every other method on a real route — not an operation.
						continue
					}
					path := prefix
					if sub != "" {
						path += "/" + sub
					}
					key := method + " " + path
					if !documented[key] {
						missing = append(missing, key+"  (routed in "+fn.Name.Name+")")
					}
				}
				return true
			})
			return false
		})
	}
	for _, m := range missing {
		t.Errorf("ROUTED BUT NOT DOCUMENTED: %s\n"+
			"    Add it to apiOps in openapi.go and regenerate. A published contract that "+
			"omits a live endpoint is how a consumer comes to believe the engine cannot do "+
			"something it does.", m)
	}
}

// subAndMethod pulls ("files", "GET") out of `sub == "files" && r.Method ==
// http.MethodGet`, and ("", "PATCH") out of `sub == "" && r.Method ==
// http.MethodPatch`. Returns ok=false for anything else.
//
// A `(r.Method == A || r.Method == B)` arm yields the FIRST method only, and that is
// deliberate: the alias arms in §6.1 (`PUT`-or-`POST` on the streaming routes) are one
// operation with two spellings, and the document describes the canonical one.
func subAndMethod(e ast.Expr) (sub, method string, ok bool) {
	bin, isBin := e.(*ast.BinaryExpr)
	if !isBin {
		return "", "", false
	}
	if bin.Op == token.LAND {
		s, _, okL := subAndMethod(bin.X)
		_, m, okR := subAndMethod(bin.Y)
		if !okL {
			return "", "", false
		}
		if !okR {
			// `sub == "x" && <something else>`: a routed sub with no single method.
			return s, "", true
		}
		return s, m, true
	}
	if bin.Op == token.LOR {
		// Alias arm: take the first method.
		_, m, okL := subAndMethod(bin.X)
		return "", m, okL
	}
	if bin.Op != token.EQL {
		return "", "", false
	}
	// sub == "<literal>"
	if id, isID := bin.X.(*ast.Ident); isID && id.Name == "sub" {
		lit, isLit := bin.Y.(*ast.BasicLit)
		if !isLit {
			return "", "", false
		}
		return strings.Trim(lit.Value, `"`), "", true
	}
	// r.Method == http.MethodX
	if sel, isSel := bin.X.(*ast.SelectorExpr); isSel && sel.Sel.Name == "Method" {
		msel, isMSel := bin.Y.(*ast.SelectorExpr)
		if !isMSel || !strings.HasPrefix(msel.Sel.Name, "Method") {
			return "", "", false
		}
		return "", strings.ToUpper(strings.TrimPrefix(msel.Sel.Name, "Method")), true
	}
	return "", "", false
}
