package relay

import (
	"testing"

	"picam-frontend/internal/backendhttp"
	"picam-frontend/internal/config"
)

func TestManagerSetBackends(t *testing.T) {
	m, err := New(&config.Config{ICEPortMin: 51200, ICEPortMax: 51300}, backendhttp.New(0))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if _, ok := m.DefaultBackend(); ok {
		t.Fatalf("DefaultBackend() before any SetBackends should report not-found")
	}
	if got := m.Backends(); got != nil {
		t.Fatalf("Backends() before any SetBackends = %+v, want nil", got)
	}

	front := config.Backend{Name: "front", Label: "Front Yard", Host: "10.10.0.50", Port: 81}
	back := config.Backend{Name: "back", Label: "Back Yard", Host: "10.10.0.51", Port: 81}
	m.SetBackends([]config.Backend{front, back})

	if got, ok := m.DefaultBackend(); !ok || got != front {
		t.Fatalf("DefaultBackend() = %+v, %v, want %+v, true", got, ok, front)
	}
	if got, ok := m.FindBackend("back"); !ok || got != back {
		t.Fatalf("FindBackend(\"back\") = %+v, %v, want %+v, true", got, ok, back)
	}
	if _, ok := m.FindBackend("missing"); ok {
		t.Fatalf("FindBackend(\"missing\") should report not-found")
	}
	if got := m.Backends(); len(got) != 2 {
		t.Fatalf("Backends() = %+v, want 2 entries", got)
	}

	// A later discovery cycle that finds fewer backends fully replaces
	// the set, not merges into it.
	m.SetBackends([]config.Backend{back})
	if got := m.Backends(); len(got) != 1 || got[0] != back {
		t.Fatalf("Backends() after replace = %+v, want [%+v]", got, back)
	}
	if _, ok := m.FindBackend("front"); ok {
		t.Fatalf("FindBackend(\"front\") should report not-found after replace")
	}
}
