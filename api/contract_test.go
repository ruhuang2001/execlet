package api_test

import (
	"context"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/api"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
	"gopkg.in/yaml.v3"
)

type specSchema struct {
	Required   []string              `yaml:"required"`
	Properties map[string]yaml.Node  `yaml:"properties"`
	AllOf      []specSchemaRefOrPart `yaml:"allOf"`
}

type specSchemaRefOrPart struct {
	Ref        string               `yaml:"$ref"`
	Required   []string             `yaml:"required"`
	Properties map[string]yaml.Node `yaml:"properties"`
}

func resolveSchema(schemas map[string]specSchema, name string) (props map[string]yaml.Node, required map[string]bool) {
	props = make(map[string]yaml.Node)
	required = make(map[string]bool)
	s := schemas[name]
	for _, r := range s.Required {
		required[r] = true
	}
	for k, v := range s.Properties {
		props[k] = v
	}
	for _, part := range s.AllOf {
		if refName, ok := strings.CutPrefix(part.Ref, "#/components/schemas/"); ok {
			subProps, subReq := resolveSchema(schemas, refName)
			for k, v := range subProps {
				props[k] = v
			}
			for r := range subReq {
				required[r] = true
			}
		}
		for _, r := range part.Required {
			required[r] = true
		}
		for k, v := range part.Properties {
			props[k] = v
		}
	}
	return props, required
}

func TestSpecFieldsMatchRequestAndSandboxTypes(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]specSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.Spec(), &doc); err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}
	for _, tc := range []struct {
		schema        string
		value         any
		checkRequired bool
	}{
		{"CreateSandboxRequest", daemon.CreateRequest{}, false},
		{"UpdateSandboxRequest", daemon.UpdateRequest{}, false},
		{"UpdateSandboxRequest", sandbox.UpdateSandboxRequest{}, false},
		{"CreateSandboxRequest", sandbox.CreateSandboxRequest{}, false},
		{"ExecuteRequest", daemon.ExecuteRequest{}, false},
		{"ExecuteRequest", sandbox.ExecuteRequest{}, false},
		{"Sandbox", daemon.Sandbox{}, true},
		{"Sandbox", sandbox.Sandbox{}, true},
		{"ExecutionSummary", daemon.ExecutionSummary{}, true},
		{"Execution", daemon.Execution{}, true},
		{"Execution", sandbox.Execution{}, false},
		{"FileEntry", daemon.FileEntry{}, true},
		{"FileEntry", sandbox.FileEntry{}, true},
	} {
		typ := reflect.TypeOf(tc.value)
		t.Run(tc.schema+"/"+typ.PkgPath(), func(t *testing.T) {
			properties, required := resolveSchema(doc.Components.Schemas, tc.schema)
			fields := make(map[string]bool)
			for i := range typ.NumField() {
				tagParts := strings.Split(typ.Field(i).Tag.Get("json"), ",")
				name := tagParts[0]
				if name == "" || name == "-" {
					continue
				}
				fields[name] = true
				if _, ok := properties[name]; !ok {
					t.Errorf("OpenAPI %s is missing field %s", tc.schema, name)
				}
				if tc.checkRequired {
					omitted := strings.Contains(typ.Field(i).Tag.Get("json"), ",omitempty") ||
						strings.Contains(typ.Field(i).Tag.Get("json"), ",omitzero")
					if required[name] && omitted {
						t.Errorf("%s.%s has omitempty/omitzero but is required in OpenAPI %s", typ, name, tc.schema)
					}
					if !required[name] && !omitted && typ.Field(i).Type.Kind() != reflect.Pointer {
						t.Errorf("%s.%s lacks omitempty/omitzero/pointer but is optional in OpenAPI %s", typ, name, tc.schema)
					}
				}
			}
			for name := range properties {
				if !fields[name] {
					t.Errorf("%s is missing OpenAPI field %s", typ, name)
				}
			}
		})
	}
}

func TestSpecMatchesAPIMD(t *testing.T) {
	want := operationsInAPIMD(t)
	got, err := api.Operations()
	if err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}

	missing, extra := api.Diff(want, got)
	for _, op := range missing {
		t.Errorf("docs/api.md documents %s but api/openapi.yaml does not declare it", op)
	}
	for _, op := range extra {
		t.Errorf("api/openapi.yaml declares %s but docs/api.md does not document it", op)
	}
}

var endpointHeading = regexp.MustCompile("`(GET|POST|PUT|DELETE|PATCH) ([^`]+)`")

func operationsInAPIMD(t *testing.T) []api.Operation {
	t.Helper()
	data, err := os.ReadFile("../docs/api.md")
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}

	var ops []api.Operation
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		for _, m := range endpointHeading.FindAllStringSubmatch(line, -1) {
			path := m[2]
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			ops = append(ops, api.Operation{Method: m[1], Path: path})
		}
	}
	if len(ops) == 0 {
		t.Fatal("docs/api.md documents no endpoints; the Endpoints headings changed format")
	}
	return ops
}

func TestServerRoutesMatchSpec(t *testing.T) {
	specOps, err := api.Operations()
	if err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}

	routes := daemon.Routes()
	served := make([]api.Operation, 0, len(routes))
	be := membackend.New()
	mux := daemon.NewHandler(daemon.New(be), "test-version")

	for _, route := range routes {
		op := api.Operation{Method: route.Method, Path: route.Path}
		served = append(served, op)

		req := httptest.NewRequestWithContext(context.Background(), route.Method, concretePath(route.Path), nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("route %s is listed in Routes but not reachable through the server mux", op)
		}
	}

	missing, extra := api.Diff(specOps, served)
	for _, op := range missing {
		t.Errorf("spec declares %s but the server registers no route for it", op)
	}
	for _, op := range extra {
		t.Errorf("server registers %s but the spec does not declare it", op)
	}
}

func concretePath(path string) string {
	return strings.NewReplacer(
		"{sandbox_id}", "sbx_test",
		"{execution_id}", "exe_test",
		"{pid}", "1",
	).Replace(path)
}

func TestDiffReportsBothDirections(t *testing.T) {
	want := []api.Operation{
		{Method: "GET", Path: "/a"},
		{Method: "POST", Path: "/a"},
	}
	got := []api.Operation{
		{Method: "GET", Path: "/a"},
		{Method: "DELETE", Path: "/b"},
	}

	missing, extra := api.Diff(want, got)
	if len(missing) != 1 || missing[0].String() != "POST /a" {
		t.Fatalf("missing = %v, want [POST /a]", missing)
	}
	if len(extra) != 1 || extra[0].String() != "DELETE /b" {
		t.Fatalf("extra = %v, want [DELETE /b]", extra)
	}
}

func TestOperationsCoversEveryDocumentedEndpoint(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}
	if len(ops) != 23 {
		t.Fatalf("parsed %d operations, want 23: %v", len(ops), ops)
	}
}
