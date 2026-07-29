package maniflextest

import (
	"fmt"
	"net/http"
	"testing"
)

// Fixture describes one named record created through the public API.
type Fixture struct {
	Name    string
	Path    string
	Body    any
	Options []RequestOption
}

// Fixtures indexes the created response data by fixture name.
type Fixtures map[string]map[string]any

// Seed creates fixtures in order and returns their response data by name.
func (s *Server) Seed(fixtures ...Fixture) Fixtures {
	s.t.Helper()
	created := make(Fixtures, len(fixtures))
	for _, fixture := range fixtures {
		if fixture.Name == "" {
			s.fatalf("maniflextest: fixture name must not be empty")
		}
		if fixture.Path == "" {
			s.fatalf("maniflextest: fixture %q path must not be empty", fixture.Name)
		}
		if _, exists := created[fixture.Name]; exists {
			s.fatalf("maniflextest: duplicate fixture name %q", fixture.Name)
		}
		response := s.POST(fixture.Path, fixture.Body, fixture.Options...)
		response.AssertStatus(http.StatusCreated)
		created[fixture.Name] = response.Data()
	}
	return created
}

// Factory builds count fixtures from a deterministic zero-based index.
func Factory[T any](namePrefix, path string, count int, build func(int) T, opts ...RequestOption) []Fixture {
	if count < 0 {
		panic("maniflextest: fixture factory count must not be negative")
	}
	fixtures := make([]Fixture, count)
	for i := range count {
		fixtures[i] = Fixture{
			Name:    fmt.Sprintf("%s[%d]", namePrefix, i),
			Path:    path,
			Body:    build(i),
			Options: append([]RequestOption(nil), opts...),
		}
	}
	return fixtures
}

// Record returns a named fixture or fails the test.
func (f Fixtures) Record(t testing.TB, name string) map[string]any {
	t.Helper()
	record, ok := f[name]
	if !ok {
		t.Fatalf("maniflextest: fixture %q does not exist", name)
	}
	return record
}

// ID returns the string ID of a named fixture or fails the test.
func (f Fixtures) ID(t testing.TB, name string) string {
	t.Helper()
	id, ok := f.Record(t, name)["id"].(string)
	if !ok {
		t.Fatalf("maniflextest: fixture %q has no string id", name)
	}
	return id
}
