//go:build ignore

// realtime_example.go wires the realtime.Hub onto the event bus: every model
// mutation is published via events.Emit and fanned out to connected WebSocket
// and SSE clients. It also serves an AsyncAPI 2.6 document describing the event
// channels, and enables resumable streams (Last-Event-ID).
//
// Run with:
//
//	go run ./examples/realtime.go
//
// Then, in a browser console on http://localhost:8082:
//
//	const es = new EventSource("/sse?subscribe=order.*");
//	es.onmessage = (m) => console.log(JSON.parse(m.data));
//
// and create an order to watch it arrive live:
//
//	fetch("/api/orders", {method:"POST", headers:{"Content-Type":"application/json"},
//	    body: JSON.stringify({reference:"A-1", total:42})});
//
// The AsyncAPI document is at http://localhost:8082/api/asyncapi.json.
package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// Order is the model whose changes drive realtime events. The default
// events.Emit naming turns a create into "order.created", etc.
type Order struct {
	maniflex.BaseModel
	Reference string  `json:"reference" mfx:"required,filterable"`
	Total     float64 `json:"total"`
	Status    string  `json:"status" mfx:"enum:pending|paid|shipped,filterable"`
}

// PaymentReceived is a custom (non-model) event payload. Declaring it on the
// AsyncAPI document lets clients codegen its type even though it is published by
// hand rather than by the CRUD pipeline.
type PaymentReceived struct {
	OrderID string  `json:"order_id" mfx:"required"`
	Amount  float64 `json:"amount"`
	Method  string  `json:"method" mfx:"enum:card|cash"`
}

func main() {
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(Order{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		log.Fatal(err)
	}
	server.SetDB(db)

	// One in-process bus shared by the producer (events.Emit) and the consumer
	// (the hub). Swap inproc for events/redis or events/nats to fan out across
	// replicas.
	bus := inproc.New()

	// Producer: publish a domain event after every create/update/delete.
	server.Pipeline.DB.Register(
		events.Emit(bus, events.EmitConfig{Source: "shop"}),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.AtPosition(maniflex.After),
	)

	// Consumer: fan events out to WS + SSE clients, with a 1024-event replay
	// buffer so reconnecting clients resume via Last-Event-ID.
	hub, err := realtime.NewHub(realtime.HubConfig{
		Bus:          bus,
		ResumeBuffer: 1024,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Publish an AsyncAPI 2.6 document at /api/asyncapi.json.
	server.RealtimeDoc(maniflex.AsyncAPIConfig{
		Title:           "Shop events",
		AutoModelEvents: true,
		Servers: []maniflex.AsyncAPIServerConfig{
			{Name: "ws", URL: "ws://localhost:8082/ws", Protocol: "ws"},
			{Name: "sse", URL: "http://localhost:8082/sse", Protocol: "http"},
		},
		Events: []maniflex.EventDoc{
			{Type: "payment.received", Title: "Payment received", Payload: PaymentReceived{}},
		},
	})

	// The maniflex server owns /api/*; the hub is mounted alongside it.
	mux := http.NewServeMux()
	mux.Handle("/api/", server.Handler())
	mux.Handle("/ws", hub.Handler())
	mux.Handle("/sse", hub.SSEHandler())

	fmt.Println("Listening on :8082")
	fmt.Println("  REST:     POST /api/orders")
	fmt.Println("  WS:       ws://localhost:8082/ws")
	fmt.Println("  SSE:      http://localhost:8082/sse?subscribe=order.*")
	fmt.Println("  AsyncAPI: http://localhost:8082/api/asyncapi.json")
	if err := http.ListenAndServe(":8082", mux); err != nil {
		log.Fatal(err)
	}
}
