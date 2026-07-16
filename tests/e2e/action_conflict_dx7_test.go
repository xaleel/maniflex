package e2e

// checkActionConflict used to know only the five CRUD routes, so an action that
// shadowed /{model}/export, /{model}/aggregate, an attachment path, or a
// singleton's PATCH passed registration. Because each model is mounted under its
// own sub-tree the collision does not panic when the router builds — it silently
// shadows, and the model's endpoint quietly stops answering. The check now
// enumerates the real mounted surface and flags an action that resolves the same
// path *and* method as a model route. A parameter's name does not matter (the
// router keys on position), and a method the model does not serve at that path is
// free to take — both confirmed against the router's actual routing (DX-7).

import (
	"fmt"
	"testing"

	"github.com/xaleel/maniflex"
)

type dx7Order struct {
	maniflex.BaseModel
	Name string `json:"name" mfx:"filterable"`
}

type dx7Config struct {
	maniflex.BaseModel
	Theme string `json:"theme" mfx:"default:light"`
}

type dx7Doc struct {
	maniflex.BaseModel
	Avatar string `json:"avatar" mfx:"file"`
}

// registers builds a server preloaded with a model, ready for an Action() call.
type registers func(*maniflex.Server)

func ordersPlain(s *maniflex.Server) {
	s.MustRegister(dx7Order{}, maniflex.ModelConfig{TableName: "orders"})
}
func ordersExportAgg(s *maniflex.Server) {
	s.MustRegister(dx7Order{}, maniflex.ModelConfig{
		TableName: "orders", ExportEnabled: true, AggregateEnabled: true,
	})
}
func configSingleton(s *maniflex.Server) {
	s.MustRegister(dx7Config{}, maniflex.ModelConfig{TableName: "app_config", Singleton: true})
}
func docsWithFile(s *maniflex.Server) {
	s.MustRegister(dx7Doc{}, maniflex.ModelConfig{TableName: "docs"})
}

// actionPanics registers the model, then registers an action and reports whether
// Action() panicked (and its message).
func actionPanics(t *testing.T, reg registers, method, path string) (panicked bool, msg string) {
	t.Helper()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	reg(srv)
	defer func() {
		if r := recover(); r != nil {
			panicked, msg = true, fmt.Sprint(r)
		}
	}()
	srv.Action(maniflex.ActionConfig{
		Method:  method,
		Path:    path,
		Handler: func(*maniflex.ServerContext) error { return nil },
	})
	return
}

func TestActionConflict_MountedRouteShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reg    registers
		method string
		path   string
		want   bool // want a panic
	}{
		// The shapes the old check missed.
		{"export route", ordersExportAgg, "GET", "/orders/export", true},
		{"aggregate route", ordersExportAgg, "GET", "/orders/aggregate", true},
		{"attachment route", docsWithFile, "GET", "/docs/{id}/avatar", true},
		{"attachment, param renamed", docsWithFile, "GET", "/docs/{docId}/avatar", true},
		{"singleton PATCH", configSingleton, "PATCH", "/app_config", true},
		{"singleton GET", configSingleton, "GET", "/app_config", true},

		// Item route under a different parameter name, GET — the method overlaps the
		// model's read, so the action shadows it (the router keys on position, so
		// the renamed parameter still lands on the same route).
		{"item, param renamed, method overlaps", ordersPlain, "GET", "/orders/{orderId}", true},

		// Regressions: the cases the old check already caught.
		{"collection create", ordersPlain, "POST", "/orders", true},
		{"item read", ordersPlain, "GET", "/orders/{id}", true},

		// Must NOT be rejected — each resolves a request the model does not serve,
		// so nothing is shadowed (all verified against the router's real routing).
		{"item, param renamed, method the model omits", ordersPlain, "POST", "/orders/{orderId}", false},
		{"sub-action, param renamed", ordersPlain, "POST", "/orders/{orderId}/reship", false},
		{"export path, export disabled", ordersPlain, "GET", "/orders/export", false},
		{"different base", ordersExportAgg, "GET", "/invoices/export", false},
		{"sub-action, param matches", ordersPlain, "POST", "/orders/{id}/reship", false},
		{"new method on item, param matches", ordersPlain, "POST", "/orders/{id}", false},
		{"singleton has no id subtree", configSingleton, "GET", "/app_config/{id}", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			panicked, msg := actionPanics(t, tc.reg, tc.method, tc.path)
			switch {
			case tc.want && !panicked:
				t.Errorf("Action(%s %s) was accepted, but it resolves the same path and method as a "+
					"model route and would silently shadow it — the conflict must be caught here, "+
					"at the Action() call", tc.method, tc.path)
			case !tc.want && panicked:
				t.Errorf("Action(%s %s) was rejected (%s), but it does not shadow any mounted route",
					tc.method, tc.path, msg)
			}
		})
	}
}
