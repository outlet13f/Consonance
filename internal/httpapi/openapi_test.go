package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// openAPIDoc is the served document, decoded.
func openAPIDoc(t *testing.T) map[string]any {
	t.Helper()
	srv, _, _ := newTestServer(t)
	resp, body := callWith(t, srv, http.MethodGet, "/v1/openapi.json", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/openapi.json = %d %s", resp.StatusCode, body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	return doc
}

// at walks the decoded document by keys; a missing step fails the test.
func at(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: not an object at %q", strings.Join(keys, "."), k)
		}
		if v, ok = m[k]; !ok {
			t.Fatalf("%s: no %q", strings.Join(keys, "."), k)
		}
	}
	return v
}

func TestOpenAPIIsServedWithoutKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, body := callWith(t, srv, http.MethodGet, "/v1/openapi.json", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	doc := openAPIDoc(t)
	if got := at(t, doc, "openapi"); got != "3.1.0" {
		t.Errorf("openapi = %v, want 3.1.0", got)
	}
	if got := at(t, doc, "info", "title"); got != "Consonance API" {
		t.Errorf("info.title = %v", got)
	}
	if got := at(t, doc, "info", "version"); got != "v1alpha1" {
		t.Errorf("info.version = %v", got)
	}
	scheme := at(t, doc, "components", "securitySchemes", "adminKey").(map[string]any)
	if scheme["type"] != "http" || scheme["scheme"] != "bearer" {
		t.Errorf("securitySchemes.adminKey = %v, want http bearer", scheme)
	}
	if got, _ := json.Marshal(at(t, doc, "security")); string(got) != `[{"adminKey":[]}]` {
		t.Errorf("security = %s, want [{adminKey: []}]", got)
	}
}

// documentedRoutes is the (method, path) pairs of the document's paths, as "METHOD /path".
func documentedRoutes(t *testing.T, doc map[string]any) (pairs []string, ops map[string]string) {
	t.Helper()
	ops = map[string]string{}
	for path, item := range at(t, doc, "paths").(map[string]any) {
		for method, op := range item.(map[string]any) {
			if method == "parameters" || method == "summary" || method == "description" {
				continue
			}
			key := strings.ToUpper(method) + " " + path
			pairs = append(pairs, key)
			ops[key], _ = op.(map[string]any)["operationId"].(string)
		}
	}
	sort.Strings(pairs)
	return pairs, ops
}

func TestOpenAPICoversEveryRoute(t *testing.T) {
	cfg, _, _ := testConfig()
	var want []string
	wantOps := map[string]string{}
	for _, r := range newServer(cfg).routes() {
		if r.Pattern == "/healthz" || r.Pattern == "/v1/openapi.json" {
			continue
		}
		key := r.Method + " " + r.Pattern
		want = append(want, key)
		wantOps[key] = r.Op
	}
	sort.Strings(want)

	got, gotOps := documentedRoutes(t, openAPIDoc(t))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the document's paths differ from the routes:\n document %v\n routes   %v", got, want)
	}
	// The operation id is the name the request log uses, so a log line leads to its operation.
	for key, op := range wantOps {
		if gotOps[key] != op {
			t.Errorf("%s: operationId = %q, want %q", key, gotOps[key], op)
		}
	}
}

// TestOpenAPIPathParameters: every {name} of a path is a declared path parameter.
func TestOpenAPIPathParameters(t *testing.T) {
	doc := openAPIDoc(t)
	params := at(t, doc, "components", "parameters").(map[string]any)
	resolve := func(p any) map[string]any {
		m := p.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			return params[strings.TrimPrefix(ref, "#/components/parameters/")].(map[string]any)
		}
		return m
	}
	for path, item := range at(t, doc, "paths").(map[string]any) {
		declared := map[string]bool{}
		list, _ := item.(map[string]any)["parameters"].([]any)
		for _, p := range list {
			if q := resolve(p); q["in"] == "path" {
				declared[q["name"].(string)] = true
			}
		}
		for _, seg := range strings.Split(path, "/") {
			if strings.HasPrefix(seg, "{") && !declared[strings.Trim(seg, "{}")] {
				t.Errorf("%s: path parameter %s is not declared", path, seg)
			}
		}
		if len(declared) != strings.Count(path, "{") {
			t.Errorf("%s: declares %d path parameters for %d in the path", path, len(declared), strings.Count(path, "{"))
		}
	}
}

func TestOpenAPIRefsResolve(t *testing.T) {
	doc := openAPIDoc(t)
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if ref, ok := x.(string); ok && k == "$ref" {
					target := any(doc)
					for _, step := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
						m, _ := target.(map[string]any)
						if target = m[step]; target == nil {
							t.Errorf("%s: %s does not resolve", where, ref)
							break
						}
					}
					continue
				}
				walk(where+"/"+k, x)
			}
		case []any:
			for i, x := range v {
				walk(fmt.Sprintf("%s/%d", where, i), x)
			}
		}
	}
	walk("", doc)
}

func TestOpenAPIProblemTypes(t *testing.T) {
	doc := openAPIDoc(t)
	var got []string
	for _, v := range at(t, doc, "components", "schemas", "Problem", "properties", "type", "enum").([]any) {
		got = append(got, v.(string))
	}
	want := []string{
		string(typeValidation), string(typeUnauthorized), string(typeNotFound), string(typeConflict),
		string(typeRateLimited), string(typeInsufficientStorage), string(typeInternal),
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Problem.type enum = %v, want %v", got, want)
	}
	if len(want) != 7 {
		t.Fatalf("the test lists %d problem types, want seven", len(want))
	}
}

// TestOpenAPISchemasMatchTheViews checks each schema against the Go type the handler writes (or
// reads): the same JSON names, the same types, and required exactly where the field has no
// omitempty or omitzero. A field added to a view and not to the document fails here.
func TestOpenAPISchemasMatchTheViews(t *testing.T) {
	doc := openAPIDoc(t)
	schemas := at(t, doc, "components", "schemas").(map[string]any)
	for name, typ := range map[string]reflect.Type{
		"Project":                   reflect.TypeFor[projectView](),
		"Branch":                    reflect.TypeFor[branchView](),
		"RestorePoint":              reflect.TypeFor[restorePointView](),
		"Connection":                reflect.TypeFor[connectionView](),
		"RestoreResult":             reflect.TypeFor[restoreResult](),
		"CreateProjectRequest":      reflect.TypeFor[createProjectRequest](),
		"CreateBranchRequest":       reflect.TypeFor[createBranchRequest](),
		"CreateRestorePointRequest": reflect.TypeFor[createRestorePointRequest](),
		"RollbackRequest":           reflect.TypeFor[rollbackRequest](),
		"Problem":                   reflect.TypeFor[problem](),
	} {
		t.Run(name, func(t *testing.T) {
			s, ok := schemas[name].(map[string]any)
			if !ok {
				t.Fatalf("no schema %s", name)
			}
			checkSchema(t, doc, name, s, typ)
		})
	}
}

// deref follows a $ref of the document.
func deref(t *testing.T, doc map[string]any, s map[string]any) map[string]any {
	t.Helper()
	ref, ok := s["$ref"].(string)
	if !ok {
		return s
	}
	var v any = doc
	for _, step := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		v = v.(map[string]any)[step]
	}
	return v.(map[string]any)
}

func checkSchema(t *testing.T, doc map[string]any, where string, s map[string]any, typ reflect.Type) {
	t.Helper()
	s = deref(t, doc, s)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var kind string
	switch {
	case typ == reflect.TypeFor[time.Time]():
		if s["type"] != "string" || s["format"] != "date-time" {
			t.Errorf("%s: want a date-time string, got %v", where, s)
		}
		return
	case typ.Kind() == reflect.String:
		kind = "string"
	case typ.Kind() == reflect.Bool:
		kind = "boolean"
	case typ.Kind() == reflect.Int32, typ.Kind() == reflect.Int64, typ.Kind() == reflect.Int:
		kind = "integer"
	case typ.Kind() == reflect.Slice:
		kind = "array"
	case typ.Kind() == reflect.Struct:
		kind = "object"
	default:
		t.Fatalf("%s: the test does not know %s", where, typ)
	}
	if s["type"] != kind {
		t.Errorf("%s: type = %v, want %s", where, s["type"], kind)
		return
	}
	if kind == "array" {
		checkSchema(t, doc, where+"[]", s["items"].(map[string]any), typ.Elem())
		return
	}
	if kind != "object" {
		return
	}
	props, _ := s["properties"].(map[string]any)
	required := map[string]bool{}
	for _, r := range anySlice(s["required"]) {
		required[r.(string)] = true
	}
	fields := map[string]bool{}
	isRequest := strings.HasSuffix(typ.Name(), "Request")
	for f := range typ.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		fields[name] = true
		prop, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("%s: no property %q", where, name)
			continue
		}
		// A response field is required unless it can be left out of the JSON. A request field has
		// no such tag to go by (parent has none and is optional), so only the names are checked.
		optional := strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
		if !isRequest && required[name] == optional {
			t.Errorf("%s.%s: required = %v, but the field's omitempty is %v", where, name, required[name], optional)
		}
		checkSchema(t, doc, where+"."+name, prop, f.Type)
	}
	for name := range props {
		if !fields[name] {
			t.Errorf("%s: property %q is not a field of %s", where, name, typ)
		}
	}
	for name := range required {
		if !fields[name] {
			t.Errorf("%s: required %q is not a field of %s", where, name, typ)
		}
	}
	// Requests refuse unknown fields (decodeBody), and the document says so.
	if isRequest && s["additionalProperties"] != false {
		t.Errorf("%s: a request body should say additionalProperties: false", where)
	}
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}
