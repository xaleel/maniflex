// Package conformance pins the framework interface each adapter module
// satisfies.
//
// It exists because satisfying an interface in Go needs no import of whoever
// declared it, so an adapter can drift out of conformance while still
// compiling, still passing its own tests, and still passing lint. The break
// then surfaces in a consumer's build rather than in this repository's.
//
// middleware/db/redis is the clearest case: its go.mod requires only go-redis,
// so nothing links it to maniflex at all. The event adapters do import events,
// which catches a changed type but not a changed method set — nothing in the
// repository assigns one to events.Bus.
//
// These assertions belong here rather than in the adapters because the tests
// module is not published: neither side gains a dependency, and
// middleware/db/redis stays free of the framework it plugs into.
//
// A break is a BUILD failure of this package, not a failing assertion — that is
// the intended behaviour, and it fails CI the same way.
package conformance

import (
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/kafka"
	"github.com/xaleel/maniflex/events/nats"
	"github.com/xaleel/maniflex/events/rabbitmq"
	eventsredis "github.com/xaleel/maniflex/events/redis"
	mdb "github.com/xaleel/maniflex/middleware/db"
	dbredis "github.com/xaleel/maniflex/middleware/db/redis"
)

var (
	// The module with no maniflex dependency of any kind.
	_ mdb.RateLimitBackend = (*dbredis.RateLimitBackend)(nil)

	// The event adapters. None of them asserted this for itself.
	_ events.Bus = (*rabbitmq.Bus)(nil)
	_ events.Bus = (*kafka.Bus)(nil)
	_ events.Bus = (*nats.Bus)(nil)
	_ events.Bus = (*eventsredis.Bus)(nil)
)
