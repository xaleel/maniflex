package maniflex

import (
	"fmt"
	"sync"
)

// Registry is a thread-safe, ordered store of registered ModelMeta values.
type Registry struct {
	mu     sync.RWMutex
	models map[string]*ModelMeta
	order  []string // preserves registration order for migration
}

func newRegistry() *Registry {
	return &Registry{models: make(map[string]*ModelMeta)}
}

func (r *Registry) add(meta *ModelMeta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.models[meta.Name]; exists {
		return fmt.Errorf("maniflex: model %q is already registered", meta.Name)
	}
	r.models[meta.Name] = meta
	r.order = append(r.order, meta.Name)
	return nil
}

// Get returns the ModelMeta registered under name.
func (r *Registry) Get(name string) (*ModelMeta, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[name]
	return m, ok
}

// All returns all registered models in registration order.
func (r *Registry) All() []*ModelMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ModelMeta, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.models[name])
	}
	return out
}

// NewRegistry creates a new empty Registry. Exported for use in tests.
func NewRegistry() *Registry { return newRegistry() }

// AddForTest adds a ModelMeta to the registry. Exported for use in tests.
func (r *Registry) AddForTest(meta *ModelMeta) error { return r.add(meta) }

// ResolveManyToManyForTest runs the M2M resolution pass. Exported for use in tests.
func ResolveManyToManyForTest(reg *Registry) error { return resolveManyToMany(reg) }

// ── Package-level registry access for DB adapters ──────────────────────────
// Adapters receive the registry at construction time; this avoids a
// circular import while keeping them able to resolve related models.

// RegistryAccessor is the read-only interface DB adapters receive.
type RegistryAccessor interface {
	Get(name string) (*ModelMeta, bool)
	All() []*ModelMeta
}

// filteredRegistry exposes a subset of an underlying Registry, identified by
// model name. Used by per-model adapter routing so each adapter's AutoMigrate
// call only sees the models routed to it.
type filteredRegistry struct {
	parent RegistryAccessor
	names  map[string]struct{}
	order  []string // preserve registration order across All()
}

func newFilteredRegistry(parent RegistryAccessor, names []string) *filteredRegistry {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return &filteredRegistry{parent: parent, names: set, order: append([]string(nil), names...)}
}

func (f *filteredRegistry) Get(name string) (*ModelMeta, bool) {
	if _, ok := f.names[name]; !ok {
		return nil, false
	}
	return f.parent.Get(name)
}

func (f *filteredRegistry) All() []*ModelMeta {
	out := make([]*ModelMeta, 0, len(f.order))
	for _, n := range f.order {
		if m, ok := f.parent.Get(n); ok {
			out = append(out, m)
		}
	}
	return out
}
