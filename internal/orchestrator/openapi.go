package orchestrator

// The machine-readable half of the §6.1 control-plane contract (ADR-0008 part 1).
//
// WHY THIS IS GENERATED RATHER THAN WRITTEN. A hand-maintained OpenAPI file is a second,
// drifting account of the same protocol. So the document is built from the table below plus
// REFLECTION over the very request/response types the handlers decode and encode: a renamed
// json tag changes the document in the same commit, and openapi_test.go fails if the
// checked-in file is stale or if a listed operation is not actually routed.
//
// api-contract.md STAYS AUTHORITATIVE FOR INTENT. It says what an endpoint means, which
// refusal is which, and why — none of which a schema can carry. When they disagree, the
// document is wrong by construction (it is derived) unless the divergence is in the handler,
// which is the CLAUDE.md Relationship-rule case: stop and decide which side moves.
//
// JSON rather than YAML: both are valid OpenAPI and every generator reads JSON, while YAML
// would add a module dependency to emit a document nothing in the engine consumes.
//
// Regenerate with:
//
//	go run ./cmd/openapigen -out docs/spec/openapi.json

//go:generate go run ../../cmd/openapigen -out ../../docs/spec/openapi.json

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// openAPIVersion is the document's own version, NOT the engine's. It moves when the
// §6.1 surface changes shape, which a consumer's generated client cares about; the
// engine ships no version negotiation (there are no live instances to negotiate
// with — CLAUDE.md "no compatibility burden").
const openAPIVersion = "0.1.0"

// openAPIDocPath is where the generated document is checked in, relative to the
// module root. Named here rather than only in cmd/openapigen's flag default because
// the staleness guard has to read the same file the generator writes — two literals
// would let the guard pass while checking a path nobody publishes.
const openAPIDocPath = "docs/spec/openapi.json"

// apiParam is one query parameter. Required is false throughout §6.1's control
// plane: every query parameter it defines has a documented default.
type apiParam struct {
	Name string
	Type string // "string" | "integer" | "boolean"
	Desc string
}

// apiOp is one §6.1 operation. Req/Resp hold a ZERO VALUE of the wire type, not a
// hand-written schema — that is what ties the document to the handlers rather than
// to somebody's memory of them.
type apiOp struct {
	Method  string
	Path    string
	OpID    string
	Summary string
	Query   []apiParam
	Req     any
	Resp    any
	Status  int
	// Errors are the status codes §6.2 defines for this operation. Listed rather
	// than derived: which refusal an endpoint can produce is a design fact stated in
	// api-contract.md, and no amount of reflection recovers it from a struct.
	Errors []int
}

// apiOps is the §6.1 control plane, in the document's order.
//
// THE DATA PLANE IS ABSENT ON PURPOSE, and it is not a gap: process and fs are
// Connect-RPC over `khora.transport.v1` (transport D5), whose machine-readable
// contract is the .proto itself. The two REST routes that DO appear here —
// `/files` and the volume import/export pair — are the streaming bypasses (D4.4),
// REST precisely because a file must not become one protobuf message.
//
// `/healthz`, `/readyz` and `/metrics` are absent for a different reason: they are
// the operational surface, not §6.1. A consumer builds against the API; an operator
// scrapes those.
var apiOps = []apiOp{
	{
		Method: "POST", Path: "/v1/sandboxes", OpID: "createSandbox",
		Summary: "Create a sandbox. Unknown JSON fields are REFUSED (a misspelled `network` would otherwise silently create a deny sandbox).",
		Req:     createReq{}, Resp: createResp{}, Status: 201,
		Errors: []int{400, 404, 409, 507},
	},
	{
		Method: "GET", Path: "/v1/sandboxes", OpID: "listSandboxes",
		Summary: "List all sandboxes.",
		Resp:    []sandboxResp{}, Status: 200,
	},
	{
		Method: "GET", Path: "/v1/sandboxes/{id}", OpID: "getSandbox",
		Summary: "Inspect one sandbox. Never exposes placement (§6.2).",
		Resp:    sandboxResp{}, Status: 200, Errors: []int{404},
	},
	{
		Method: "PATCH", Path: "/v1/sandboxes/{id}", OpID: "patchSandbox",
		Summary: "Rewrite the sandbox's Binding. Every field is optional; an omitted field is left alone, and a whole binding is swapped for a whole binding.",
		Req:     patchReq{}, Resp: patchResp{}, Status: 200,
		Errors: []int{400, 404, 409, 507},
	},
	{
		Method: "DELETE", Path: "/v1/sandboxes/{id}", OpID: "destroySandbox",
		Summary: "Destroy a sandbox. The record is DELETED, so a later GET is 404 and the destroy's cause is readable only on the event (§4.4-C).",
		Status:  204, Errors: []int{404, 409},
	},
	{
		Method: "POST", Path: "/v1/sandboxes/{id}/pause", OpID: "pauseSandbox",
		Summary: "Pause a running sandbox: drains in-flight engine writes, then freezes.",
		Resp:    verbResp{}, Status: 200, Errors: []int{404, 409},
	},
	{
		Method: "POST", Path: "/v1/sandboxes/{id}/resume", OpID: "resumeSandbox",
		Summary: "Resume a paused sandbox. The filesystem is exactly as it was; the placement and IP are new.",
		Resp:    verbResp{}, Status: 200, Errors: []int{404, 409, 507},
	},
	{
		Method: "POST", Path: "/v1/sandboxes/{id}/activity", OpID: "recordActivity",
		Summary: "Record one activity NOW, stamping the guest's idle clock so the idle sweep sees the sandbox as busy. Carries no duration and grants no protected period — repeat it while the work lasts.",
		Resp:    activityResp{}, Status: 200, Errors: []int{404, 409, 503},
	},
	{
		Method: "GET", Path: "/v1/sandboxes/{id}/network", OpID: "getSandboxNetwork",
		Summary: "Read the sandbox's effective network declaration, including per-rule hit counters.",
		Resp:    networkReadResp{}, Status: 200, Errors: []int{404},
	},
	{
		Method: "POST", Path: "/v1/sandboxes/{id}/publish", OpID: "publishPort",
		Summary: "Publish a guest port to the host.",
		Req:     publishReq{}, Resp: publishResp{}, Status: 200,
		Errors: []int{400, 404, 409},
	},
	{
		Method: "GET", Path: "/v1/sandboxes/{id}/files", OpID: "downloadFile",
		Summary: "Stream a file OUT of the sandbox (the §5.E large-file bypass; REST rather than RPC because it streams bytes). Returns application/octet-stream, chunked.",
		Query:   []apiParam{{Name: "path", Type: "string", Desc: "Absolute path inside the guest."}},
		Status:  200, Errors: []int{400, 403, 404, 503},
	},
	{
		Method: "PUT", Path: "/v1/sandboxes/{id}/files", OpID: "uploadFile",
		Summary: "Stream a file INTO the sandbox. Body is the raw bytes. Written to a temp file and renamed, so an interrupted upload leaves no partial file.",
		Query:   []apiParam{{Name: "path", Type: "string", Desc: "Absolute path inside the guest."}},
		Resp:    uploadResp{}, Status: 200,
		Errors: []int{400, 403, 404, 413, 503, 507},
	},
	{
		Method: "GET", Path: "/v1/events", OpID: "listEvents",
		Summary: "Page the transactional outbox in `seq` order. At-least-once: dedupe by event `id` (§4.4-C).",
		Query: []apiParam{
			{Name: "after", Type: "integer", Desc: "Cursor: return events with seq > this. 0 or omitted starts at the earliest retained event."},
			{Name: "limit", Type: "integer", Desc: "Maximum events in this page."},
			{Name: "wait", Type: "string", Desc: "Go duration (e.g. \"20s\"). If the page would be EMPTY, hold the request open until an event arrives or this elapses. Changes no cursor semantics; a non-empty first read never waits. Clamped to a server-side ceiling."},
		},
		Resp: eventsResp{}, Status: 200, Errors: []int{400},
	},
	{
		Method: "POST", Path: "/v1/templates", OpID: "promoteTemplate",
		Summary: "Promote a sandbox to an immutable Template other sandboxes derive from.",
		Req:     promoteReq{}, Resp: promoteResp{}, Status: 201,
		Errors: []int{400, 404, 409},
	},
	{
		Method: "GET", Path: "/v1/templates", OpID: "listTemplates",
		Summary: "List all templates.",
		Resp:    []templateResp{}, Status: 200,
	},
	{
		Method: "DELETE", Path: "/v1/templates/{id}", OpID: "deleteTemplate",
		Summary: "Delete a template. Refused while any sandbox references it (§4.3 reference protection).",
		Status:  204, Errors: []int{404, 409},
	},
	{
		Method: "POST", Path: "/v1/volumes", OpID: "createVolume",
		Summary: "Create a volume. Independent of every sandbox (§5.C).",
		Req:     volumeCreateReq{}, Resp: volumeResp{}, Status: 201,
		Errors: []int{400, 409, 507},
	},
	{
		Method: "GET", Path: "/v1/volumes", OpID: "listVolumes",
		Summary: "List all volumes.",
		Resp:    []volumeResp{}, Status: 200,
	},
	{
		Method: "GET", Path: "/v1/volumes/{id}", OpID: "getVolume",
		Summary: "Inspect one volume.",
		Resp:    volumeResp{}, Status: 200, Errors: []int{404},
	},
	{
		Method: "PATCH", Path: "/v1/volumes/{id}", OpID: "patchVolume",
		Summary: "Rewrite a volume's cap.",
		Req:     volumePatchReq{}, Resp: volumeResp{}, Status: 200,
		Errors: []int{400, 404, 409, 507},
	},
	{
		Method: "DELETE", Path: "/v1/volumes/{id}", OpID: "deleteVolume",
		Summary: "Delete a volume. Refused while mounted (§4.3 reference protection).",
		Status:  204, Errors: []int{404, 409},
	},
	{
		Method: "POST", Path: "/v1/volumes/{id}/snapshot", OpID: "snapshotVolume",
		Summary: "Snapshot a volume into a new volume.",
		Resp:    volumeResp{}, Status: 201, Errors: []int{404, 409, 507},
	},
	{
		Method: "POST", Path: "/v1/volumes/{id}/clone", OpID: "cloneVolume",
		Summary: "Clone a volume into a new, independently writable volume.",
		Resp:    volumeResp{}, Status: 201, Errors: []int{404, 409, 507},
	},
	{
		Method: "POST", Path: "/v1/volumes/{id}/import", OpID: "importVolume",
		Summary: "Stream bytes INTO a volume with no sandbox involved. Body is the raw content.",
		Query:   []apiParam{{Name: "path", Type: "string", Desc: "Path inside the volume."}},
		Resp:    importResp{}, Status: 200,
		Errors: []int{400, 404, 409, 413, 507},
	},
	{
		Method: "GET", Path: "/v1/volumes/{id}/export", OpID: "exportVolume",
		Summary: "Stream bytes OUT of a volume with no sandbox involved. Returns application/octet-stream.",
		Query:   []apiParam{{Name: "path", Type: "string", Desc: "Path inside the volume."}},
		Status:  200, Errors: []int{400, 404, 409},
	},
}

// schemaFor renders one Go wire type as an OpenAPI schema, recursing into structs
// and slices and collecting named struct types into `defs` as reusable components.
//
// The json tag is the ONLY source of a property name — never the Go field name. That
// is what makes the document derived rather than parallel: renaming a tag moves the
// document, and renaming only the Go field moves nothing, which is correct because
// nothing on the wire changed.
func schemaFor(t reflect.Type, defs map[string]any) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		// A pointer field is how §6.1 spells "omitting the whole object is legal"
		// (createReq.Network is the case). The schema is the pointee's; optionality is
		// carried by absence from `required`, which this document does not emit —
		// see the note on requiredness in openAPIDoc.
		return schemaFor(t.Elem(), defs)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte is base64 on the wire, per encoding/json.
			return map[string]any{"type": "string", "format": "byte"}
		}
		return map[string]any{"type": "array", "items": schemaFor(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), defs)}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Struct:
		name := t.Name()
		if name == "" {
			// Anonymous struct: inline it. Nothing in §6.1 uses one, but a future
			// inline literal must not silently produce a $ref to "".
			return structSchema(t, defs)
		}
		if _, seen := defs[name]; !seen {
			// Placeholder FIRST, so a self-referential type terminates.
			defs[name] = map[string]any{}
			defs[name] = structSchema(t, defs)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	default:
		// Deliberately not a silent fallthrough to "object": an unhandled kind means
		// the document would describe a field it does not understand, and a consumer
		// would generate code from it. openapi_test.go turns this into a failure.
		return map[string]any{"x-unsupported-go-kind": t.Kind().String()}
	}
}

// structSchema renders a struct's exported, json-visible fields.
func structSchema(t reflect.Type, defs map[string]any) map[string]any {
	props := map[string]any{}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: not on the wire
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name // no tag: encoding/json uses the field name verbatim
		}
		if f.Anonymous && tag == "" {
			// Embedded struct with no tag is FLATTENED by encoding/json, so flatten it
			// here too rather than nesting a property the wire does not have.
			inner := structSchema(f.Type, defs)
			if ip, ok := inner["properties"].(map[string]any); ok {
				for k, v := range ip {
					props[k] = v
				}
			}
			continue
		}
		props[name] = schemaFor(f.Type, defs)
	}
	return map[string]any{"type": "object", "properties": props}
}

// errorDescriptions are §6.2's status codes in the engine's own words. Central
// rather than per-operation because §6.2 assigns each code ONE meaning across the
// whole surface — a 507 means "capacity" everywhere, never "state conflict" — and
// restating that per endpoint is how the meanings drift apart.
var errorDescriptions = map[int]string{
	400: "The request cannot be honored as asked: malformed, an unknown JSON field, or a value the engine can never satisfy.",
	403: "The guest refused the operation (a permission error inside the sandbox).",
	404: "No such object. The message names WHICH kind, so a caller does not look in the wrong place.",
	409: "State conflict: legal request, wrong state. Resolvable by acting elsewhere (resume it, raise a cap, release a reference) and retrying.",
	413: "The body exceeds the engine's per-request limit.",
	503: "The sandbox's in-guest transport is unreachable, or a pause is draining. The answer may differ later.",
	507: "Capacity: nothing is wrong with the request or the sandbox — the node cannot honor another promise.",
}

// OpenAPIDoc renders the §6.1 control plane as an OpenAPI 3.1 document.
//
// Indented, key-sorted JSON with a trailing newline, because the checked-in copy is
// diffed against this output byte-for-byte (openapi_test.go): a map iterating in a
// random order would make the guard fail at random, and Go's encoding/json sorts map
// keys, which is exactly the determinism this relies on.
func OpenAPIDoc() ([]byte, error) {
	defs := map[string]any{}
	paths := map[string]any{}

	for _, op := range apiOps {
		item, _ := paths[op.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			// Path parameters are derived from the template rather than declared, so a
			// new /{something}/ route cannot forget to document its own parameter.
			if params := pathParams(op.Path); len(params) > 0 {
				item["parameters"] = params
			}
			paths[op.Path] = item
		}

		operation := map[string]any{
			"operationId": op.OpID,
			"summary":     op.Summary,
			"responses":   responsesFor(op, defs),
		}
		if len(op.Query) > 0 {
			qs := make([]any, 0, len(op.Query))
			for _, q := range op.Query {
				qs = append(qs, map[string]any{
					"name": q.Name, "in": "query", "required": false,
					"description": q.Desc,
					"schema":      map[string]any{"type": q.Type},
				})
			}
			operation["parameters"] = qs
		}
		if op.Req != nil {
			operation["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/json": map[string]any{
						"schema": schemaFor(reflect.TypeOf(op.Req), defs),
					},
				},
			}
		} else if op.Method == "PUT" && strings.HasSuffix(op.Path, "/files") ||
			op.Method == "POST" && strings.HasSuffix(op.Path, "/import") {
			// The two streaming inbound routes carry RAW BYTES, not JSON. Stated
			// explicitly: a consumer that generated a JSON body for these would send a
			// base64 blob and write a corrupt file.
			operation["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/octet-stream": map[string]any{
						"schema": map[string]any{"type": "string", "format": "binary"},
					},
				},
			}
		}
		item[strings.ToLower(op.Method)] = operation
	}

	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "khora engine — control plane (§6.1)",
			"version": openAPIVersion,
			"description": "GENERATED from the handlers' own wire types (internal/orchestrator/openapi.go). " +
				"Do not edit by hand; regenerate with `go generate ./internal/orchestrator`. " +
				"docs/spec/api-contract.md remains authoritative for INTENT — what an endpoint means and " +
				"which refusal is which. This document says only what is on the wire.\n\n" +
				"The process and fs data plane is NOT here: it is Connect-RPC, and its machine-readable " +
				"contract is proto/khora/transport/v1/transport.proto.\n\n" +
				"No authentication is described because the engine has none by design (ADR-0007): it " +
				"refuses any listener but loopback, and auth/TLS belong to a layer outside it.",
		},
		"paths": paths,
		"components": map[string]any{
			"schemas": defs,
		},
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode openapi: %w", err)
	}
	return buf.Bytes(), nil
}

// responsesFor builds an operation's response map: the success entry plus one per
// §6.2 code it can produce.
func responsesFor(op apiOp, defs map[string]any) map[string]any {
	out := map[string]any{}
	success := map[string]any{"description": "Success."}
	switch {
	case op.Resp != nil:
		success["content"] = map[string]any{
			"application/json": map[string]any{"schema": schemaFor(reflect.TypeOf(op.Resp), defs)},
		}
	case op.Status == 204:
		success["description"] = "Success, no content."
	default:
		// The streaming outbound routes: bytes, of unknown length. No Content-Length is
		// sent — asking the guest for a size first would let the file change between
		// that answer and the transfer.
		success["content"] = map[string]any{
			"application/octet-stream": map[string]any{
				"schema": map[string]any{"type": "string", "format": "binary"},
			},
		}
	}
	out[fmt.Sprint(op.Status)] = success

	codes := append([]int{}, op.Errors...)
	// 405 is on EVERY route in the table by §6.2's general rule ("a wrong method on
	// any collection/object route → 405"), so it is added here rather than repeated
	// on 26 entries — repetition being how one of them would come to be missing.
	codes = append(codes, 405)
	sort.Ints(codes)
	for _, c := range codes {
		desc := errorDescriptions[c]
		if c == 405 {
			desc = "The route exists; this method does not apply to it (§6.2)."
		}
		out[fmt.Sprint(c)] = map[string]any{
			"description": desc,
			"content": map[string]any{
				"text/plain": map[string]any{"schema": map[string]any{"type": "string"}},
			},
		}
	}
	return out
}

// pathParams derives the `{id}`-shaped parameters from a path template.
func pathParams(path string) []any {
	var out []any
	for _, seg := range strings.Split(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.Trim(seg, "{}")
		out = append(out, map[string]any{
			"name": name, "in": "path", "required": true,
			"description": "Engine-minted object ID.",
			"schema":      map[string]any{"type": "string"},
		})
	}
	return out
}
