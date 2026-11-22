package daemon

import (
	"net/http"
	"testing"
)

func TestEveryRouteHasAHandler(t *testing.T) {
	for _, route := range routes {
		key := routeKey(route.Method, route.Path)
		if route.handle == nil {
			t.Errorf("%s has no handler", key)
			continue
		}
		if route.handle == nil {
			t.Errorf("%s bound to a nil handler", key)
		}
	}
}

func TestMissingHandlerPanicsAtBuildTime(t *testing.T) {
	h := NewHandler(nil, "test-version")
	defer func() {
		if recover() == nil {
			t.Fatal("newMux accepted a route with no handler; want a panic")
		}
	}()
	newMux(h, []Route{{Operation: Operation{Method: http.MethodGet, Path: "/v1/not-implemented"}}})
}

func TestRouteTableHasNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		key := routeKey(route.Method, route.Path)
		if seen[key] {
			t.Errorf("route %s is listed twice", key)
		}
		seen[key] = true
	}
}

func TestRoutesReturnsACopy(t *testing.T) {
	first := Routes()
	if len(first) == 0 {
		t.Fatal("route table is empty")
	}
	want := first[0]
	first[0] = Operation{Method: http.MethodGet, Path: "/v1/mutated"}

	if got := Routes()[0]; got != want {
		t.Fatalf("Routes() returned the table itself: first entry is %v, want %v", got, want)
	}
}
