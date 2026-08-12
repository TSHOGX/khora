package warren

// The guard that makes pkg/warren's duplicated wire types safe (ADR-0008 part 2, rule
// 1: every field maps 1:1 onto a §6.1 field).
//
// THE DUPLICATION IS UNAVOIDABLE. internal/orchestrator's request/response structs are
// unexported inside `internal/`, so a consumer cannot name them; exporting them would
// make the engine's internal handler shapes a published API that could then never be
// refactored. So this package restates them — and a restatement that nothing checks is
// exactly the "second, drifting account of the same thing" the repo's spec discipline
// exists to prevent.
//
// What it checks against is docs/spec/openapi.json, NOT the handler source. That is the
// load-bearing choice: the document is itself generated from the handler types by
// reflection and its own staleness guard fails if it falls behind them
// (internal/orchestrator/openapi_test.go). So the chain is
//
//	handler types → (reflection) → openapi.json → (this test) → pkg/warren types
//
// and each link is mechanical. Comparing against the handler source instead would mean
// re-implementing that reflection here, in a package that cannot import it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// openAPI is the parsed contract document, loaded once.
type openAPI struct {
	Components struct {
		Schemas map[string]struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
	// A path item's keys are the HTTP methods PLUS OpenAPI's own `parameters` array, so
	// the values cannot be typed uniformly — hence RawMessage and the httpMethods filter
	// at the one place they are read.
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

// httpMethods are the path-item keys that are operations. Enumerated rather than
// "anything that is not `parameters`", so a future path-level key cannot be mistaken for
// an operation the client is failing to cover.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "patch": true, "delete": true,
	"head": true, "options": true, "trace": true,
}

func loadContract(t *testing.T) openAPI {
	t.Helper()
	// ../../docs/spec/openapi.json — this package sits at pkg/warren.
	path := filepath.Join("..", "..", "docs", "spec", "openapi.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (generate it: go run ./cmd/openapigen)", path, err)
	}
	var doc openAPI
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatalf("%s has no schemas", path)
	}
	return doc
}

// jsonFields returns the json field names a Go type puts on the wire, flattening
// embedded structs the way encoding/json does.
func jsonFields(rt reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := range rt.NumField() {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			if f.Anonymous {
				inner := f.Type
				if inner.Kind() == reflect.Pointer {
					inner = inner.Elem()
				}
				if inner.Kind() == reflect.Struct {
					for k := range jsonFields(inner) {
						out[k] = true
					}
					continue
				}
			}
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// TestClientTypesMatchThePublishedContract is the field-by-field diff.
//
// Both directions matter and they fail differently:
//   - A field in the contract that this package LACKS is a capability a Go consumer
//     cannot reach — the engine accepts something the client cannot express.
//   - A field here that the contract does NOT have is worse: it is a field a caller
//     will set expecting an effect, which the engine's decoder then refuses outright
//     (the control plane rejects unknown fields) or silently ignores. Either way the
//     client has promised something the engine never offered.
func TestClientTypesMatchThePublishedContract(t *testing.T) {
	doc := loadContract(t)

	// Each client type paired with the contract schema it restates. The schema names
	// are the handler types' own Go names, which is how openapi.go derives them.
	cases := []struct {
		schema string
		typ    any
		// readOnly are fields the READ face emits and the write face accepts-and-ignores.
		// They are legitimately absent from a write-side schema, and carrying them is
		// what makes the read response a legal write body (§6.1's read-modify-write
		// requirement) — so their absence there is not drift.
		readOnly []string
	}{
		{schema: "createReq", typ: CreateRequest{}},
		{schema: "sandboxResp", typ: Sandbox{}},
		{schema: "patchReq", typ: PatchRequest{}},
		{schema: "verbResp", typ: VerbResult{}},
		{schema: "activityResp", typ: ActivityResult{}},
		{schema: "templateResp", typ: Template{}},
		{schema: "eventItem", typ: Event{}},
		{schema: "eventsResp", typ: EventPage{}},
		{schema: "volumeResp", typ: Volume{}},
		{schema: "volumeCreateReq", typ: VolumeCreateRequest{}},
		{schema: "volumePatchReq", typ: VolumePatchRequest{}},
		{schema: "importResp", typ: VolumeWriteResult{}},
		{schema: "volumeBindingItem", typ: VolumeBinding{}},
		{schema: "publishResp", typ: PublishResult{}},
		// The network family: this package has ONE type per object where the engine has a
		// read form and a write form, deliberately (see the Network type's doc). It is
		// therefore checked against the READ schemas, which are the supersets — the write
		// forms accept every field these carry.
		{schema: "networkReadResp", typ: Network{}},
		{schema: "egressReadWire", typ: Egress{}},
		{schema: "ingressReadWire", typ: Ingress{}},
		{schema: "tupleWire", typ: Tuple{}},
		{schema: "viaReadWire", typ: Via{}},
		{schema: "publishReadWire", typ: Publish{}},
	}

	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			schema, ok := doc.Components.Schemas[tc.schema]
			if !ok {
				var have []string
				for k := range doc.Components.Schemas {
					have = append(have, k)
				}
				sort.Strings(have)
				t.Fatalf("the contract has no schema %q — it was renamed or dropped, so this "+
					"client restates a type the engine no longer has.\nschemas: %v", tc.schema, have)
			}
			want := map[string]bool{}
			for name := range schema.Properties {
				want[name] = true
			}
			got := jsonFields(reflect.TypeOf(tc.typ))
			ro := map[string]bool{}
			for _, f := range tc.readOnly {
				ro[f] = true
			}

			for name := range want {
				if !got[name] {
					t.Errorf("MISSING: the contract's %s has %q and this client does not — a Go "+
						"consumer cannot reach it", tc.schema, name)
				}
			}
			for name := range got {
				if !want[name] && !ro[name] {
					t.Errorf("INVENTED: this client has %q and the contract's %s does not. The "+
						"control plane refuses unknown fields, so a caller setting it gets a 400 "+
						"on a field this package advertised.", name, tc.schema)
				}
			}
		})
	}
}

// TestEveryContractOperationHasAClientMethod: the CLI-shell rule from the other side.
//
// ADR-0008 makes cmd/warren a shell over this package precisely so a client method no
// command can use is visible — but that only catches methods this package HAS. The
// opposite gap, an operation the engine serves and this client cannot reach, is
// invisible from inside the CLI. So it is checked against the contract's own operation
// list.
//
// The map is explicit rather than derived from method names: several operations
// deliberately do not map one-to-one (snapshot and clone share `derive`; the two
// streaming file routes are ReadFile/WriteFile), and a name-matching heuristic would
// have to be taught each exception anyway — as data, where it is readable.
func TestEveryContractOperationHasAClientMethod(t *testing.T) {
	doc := loadContract(t)

	method := map[string]string{
		"createSandbox":     "Create",
		"listSandboxes":     "List",
		"getSandbox":        "Get",
		"patchSandbox":      "Patch",
		"destroySandbox":    "Destroy",
		"pauseSandbox":      "Pause",
		"resumeSandbox":     "Resume",
		"recordActivity":    "RecordActivity",
		"getSandboxNetwork": "Network",
		"publishPort":       "PublishPort",
		"downloadFile":      "ReadFile",
		"uploadFile":        "WriteFile",
		"listEvents":        "Events",
		"listTemplates":     "Templates",
		"promoteTemplate":   "Promote",
		"deleteTemplate":    "DeleteTemplate",
		"createVolume":      "CreateVolume",
		"listVolumes":       "Volumes",
		"getVolume":         "Volume",
		"patchVolume":       "ResizeVolume",
		"deleteVolume":      "DeleteVolume",
		"snapshotVolume":    "SnapshotVolume",
		"cloneVolume":       "CloneVolume",
		"importVolume":      "ImportVolume",
		"exportVolume":      "ExportVolume",
	}

	rt := reflect.TypeOf(&Client{})
	has := map[string]bool{}
	for i := range rt.NumMethod() {
		has[rt.Method(i).Name] = true
	}

	for path, ops := range doc.Paths {
		for verb, raw := range ops {
			if !httpMethods[verb] {
				continue
			}
			var op struct {
				OperationID string `json:"operationId"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("%s %s: %v", verb, path, err)
			}
			name, mapped := method[op.OperationID]
			if !mapped {
				t.Errorf("UNREACHABLE: the contract serves %s %s (%s) and this test does not "+
					"know which client method covers it. Add the method, then map it here — an "+
					"operation with no Go method is a capability a Go consumer cannot use.",
					strings.ToUpper(verb), path, op.OperationID)
				continue
			}
			if !has[name] {
				t.Errorf("%s is mapped to Client.%s, which does not exist", op.OperationID, name)
			}
		}
	}
}

// TestClientHoldsNoStateBetweenCalls is ADR-0008's third rule, checked structurally.
//
// "No state that outlives a call" is the rule that decides whether this is a client or
// a product layer wearing a client's name, and it is the easiest of the four to break
// by accident — a cache added for one good reason is still a cache. So the Client
// struct's fields are enumerated: the endpoint it was built for and the HTTP client it
// speaks through, and nothing else.
func TestClientHoldsNoStateBetweenCalls(t *testing.T) {
	allowed := map[string]bool{"endpoint": true, "http": true}
	rt := reflect.TypeOf(Client{})
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if !allowed[name] {
			t.Errorf("Client has a field %q. ADR-0008 rule 3: no state that outlives a call — "+
				"a cache, a lease, a cursor, or a subscriber registry here makes this a product "+
				"layer with a client's name. If the field is genuinely connection-level, add it "+
				"to this guard's allowlist WITH the reason.", name)
		}
	}
}

// TestNoBannedVocabulary applies §1.1's vocabulary boundary to this package, which
// ADR-0008 requires verbatim: it is engine-facing code in the engine repo.
//
// Checked on the exported API surface — type names, method names, and field names — the
// places where a product concept would become part of the published contract.
func TestNoBannedVocabulary(t *testing.T) {
	// §1.1's banned engine-internal entity names. `generation` is barred outright;
	// the rest are barred as NAMES here.
	banned := []string{
		"generation", "principal", "tenant", "session", "turn", "agent",
		"credential", "secret", "token", "apikey",
	}
	report := func(where, name string) {
		low := strings.ToLower(strings.ReplaceAll(name, "_", ""))
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("%s %q carries the banned engine name %q (§1.1). Product-layer "+
					"concepts do not enter the engine, and this package is engine-facing code "+
					"in the engine repo.", where, name, b)
			}
		}
	}

	rt := reflect.TypeOf(&Client{})
	for i := range rt.NumMethod() {
		report("method", rt.Method(i).Name)
	}
	for _, v := range []any{
		CreateRequest{}, Sandbox{}, PatchRequest{}, VerbResult{}, ActivityResult{},
		Template{}, Event{}, EventPage{}, Volume{}, VolumeCreateRequest{},
		VolumePatchRequest{}, VolumeWriteResult{}, VolumeBinding{}, Network{},
		Egress{}, Ingress{}, Tuple{}, Via{}, Publish{}, PublishResult{},
		EventsOptions{}, Error{},
	} {
		st := reflect.TypeOf(v)
		report("type", st.Name())
		for i := range st.NumField() {
			f := st.Field(i)
			report("field", f.Name)
			if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" {
				report("json field", tag)
			}
		}
	}
}
