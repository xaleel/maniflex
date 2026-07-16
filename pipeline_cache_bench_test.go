package maniflex

// PERF-2: a step's chain was composed on every call — six times per request — each
// one walking every registered middleware, allocating before/after/skipped slices,
// and closing over a fresh chain, although the (model, operation) set is fixed once
// the router exists. It is now composed once per pair and cached.
//
// This measures the composition alone: no DB, no HTTP, so the number is not drowned
// by a 20-row page of serialization (the end-to-end read benchmark registers no
// middleware at all, which is the one shape where this costs almost nothing).
//
//	go test -run '^$' -bench BenchmarkStepChain -benchmem

import (
	"fmt"
	"testing"
)

func benchNoop(ctx *ServerContext, next func() error) error { return next() }

// benchRegistry returns a step with n middlewares registered: half scoped to this
// model (so they compose into the chain), half scoped elsewhere (so they land in
// skipped) — the mix a real app produces.
func benchRegistry(n int) *StepRegistry {
	s := newStepRegistry("db", "default", benchNoop)
	for i := range n {
		if i%2 == 0 {
			s.Register(benchNoop, ForModel("User"), WithName("mine"))
		} else {
			s.Register(benchNoop, ForModel("Other"), WithName("theirs"))
		}
	}
	return s
}

func BenchmarkStepChain(b *testing.B) {
	for _, n := range []int{0, 4, 12} {
		// Uncached: what every request paid before the router froze the registry.
		b.Run(fmt.Sprintf("middlewares=%d/compose", n), func(b *testing.B) {
			s := benchRegistry(n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = s.compose("User", OpList)
			}
		})
		// Cached: what a request pays now.
		b.Run(fmt.Sprintf("middlewares=%d/cached", n), func(b *testing.B) {
			s := benchRegistry(n)
			s.freeze()
			_ = s.build("User", OpList) // fill
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = s.build("User", OpList)
			}
		})
	}
}
