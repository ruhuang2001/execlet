package api

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var spec []byte

var httpMethods = map[string]bool{
	"get":     true,
	"put":     true,
	"post":    true,
	"delete":  true,
	"patch":   true,
	"head":    true,
	"options": true,
	"trace":   true,
}

func Spec() []byte {
	return spec
}

type Operation struct {
	Method string
	Path   string
}

func (o Operation) String() string {
	return o.Method + " " + o.Path
}

func Operations() ([]Operation, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("api: parse embedded spec: %w", err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("api: embedded spec defines no paths")
	}

	ops := make([]Operation, 0, len(doc.Paths))
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("api: spec path %q is not absolute", path)
		}
		for key := range item {
			method := strings.ToLower(key)
			if !httpMethods[method] {
				continue
			}
			ops = append(ops, Operation{Method: strings.ToUpper(method), Path: path})
		}
	}
	sortOperations(ops)
	return ops, nil
}

func Diff(want, got []Operation) (missing, extra []Operation) {
	index := make(map[Operation]bool, len(got))
	for _, op := range got {
		index[op] = true
	}
	for _, op := range want {
		if !index[op] {
			missing = append(missing, op)
		}
		delete(index, op)
	}
	for op := range index {
		extra = append(extra, op)
	}
	sortOperations(missing)
	sortOperations(extra)
	return missing, extra
}

func sortOperations(ops []Operation) {
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
}
