package maniflex

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type Bug03PlainModel struct {
	BaseModel
	Name string `json:"name"`
}

type Bug03VersionedModel struct {
	BaseModel
	Value string `json:"value"`
}

type Bug03RaceModel struct {
	BaseModel
	Value string `json:"value"`
}

func bug03Server() *Server {
	return New(Config{
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func bug03Noop(_ *ServerContext, next func() error) error { return next() }

func TestRegisterAfterHandlerIsRejectedWithoutMutation(t *testing.T) {
	srv := bug03Server()
	h := srv.Handler()

	before := len(srv.registry.All())
	err := srv.Register(Bug03PlainModel{})
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("Register after Handler error = %v, want ErrRegistrationClosed", err)
	}
	if got := len(srv.registry.All()); got != before {
		t.Fatalf("registry contains %d models after rejected registration, want %d", got, before)
	}
	if _, ok := srv.registry.Get("Bug03PlainModel"); ok {
		t.Fatal("rejected model was added to the registry")
	}

	meta, err := ScanModel(Bug03PlainModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("scan expected model: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/"+meta.TableName, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("late model route status = %d, want 404", rec.Code)
	}

	spec := GenerateAsyncAPI(srv.registry, &srv.cfg, AsyncAPIConfig{AutoModelEvents: true})
	if _, ok := spec.Components.Schemas[meta.Name]; ok {
		t.Fatal("rejected model leaked into generated AsyncAPI schemas")
	}
}

func TestLateVersionedModelLeavesNoHistoryOrMiddlewareResidue(t *testing.T) {
	srv := bug03Server()
	_ = srv.Handler()

	authBefore := len(srv.Pipeline.Auth.middlewares)
	dbBefore := len(srv.Pipeline.DB.middlewares)
	err := srv.Register(Bug03VersionedModel{}, ModelConfig{
		Versioned: true,
		Middleware: &ModelMiddleware{
			Auth: []MiddlewareFunc{bug03Noop},
		},
	})
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("Register after Handler error = %v, want ErrRegistrationClosed", err)
	}
	for _, name := range []string{"Bug03VersionedModel", "Bug03VersionedModelHistory"} {
		if _, ok := srv.registry.Get(name); ok {
			t.Errorf("rejected registration left %q in the registry", name)
		}
	}
	if got := len(srv.Pipeline.Auth.middlewares); got != authBefore {
		t.Errorf("Auth middleware count = %d after rejection, want %d", got, authBefore)
	}
	if got := len(srv.Pipeline.DB.middlewares); got != dbBefore {
		t.Errorf("DB middleware count = %d after rejection, want %d", got, dbBefore)
	}
}

func TestLateComputedFieldDoesNotMutateModelMetadata(t *testing.T) {
	srv := bug03Server()
	if err := srv.Register(Bug03PlainModel{}); err != nil {
		t.Fatalf("Register before Handler: %v", err)
	}
	_ = srv.Handler()

	meta, _ := srv.registry.Get("Bug03PlainModel")
	before := len(meta.Computed)
	err := srv.AddComputedField("Bug03PlainModel", "late_value",
		func(*ServerContext, map[string]any) (any, error) { return "late", nil })
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("AddComputedField after Handler error = %v, want ErrRegistrationClosed", err)
	}
	if got := len(meta.Computed); got != before {
		t.Fatalf("computed field count = %d after rejection, want %d", got, before)
	}
}

func TestRegisterAndHandlerRaceHasOneConsistentOutcome(t *testing.T) {
	expected, err := ScanModel(Bug03RaceModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("scan expected model: %v", err)
	}

	for i := 0; i < 64; i++ {
		srv := bug03Server()
		start := make(chan struct{})
		registerDone := make(chan error, 1)
		handlerDone := make(chan http.Handler, 1)

		go func() {
			<-start
			registerDone <- srv.Register(Bug03RaceModel{})
		}()
		go func() {
			<-start
			handlerDone <- srv.Handler()
		}()
		close(start)

		registerErr := <-registerDone
		h := <-handlerDone
		_, registered := srv.registry.Get(expected.Name)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/"+expected.TableName, nil))
		_, documented := GenerateAsyncAPI(
			srv.registry,
			&srv.cfg,
			AsyncAPIConfig{AutoModelEvents: true},
		).Components.Schemas[expected.Name]

		switch {
		case registerErr == nil:
			if !registered || !documented || rec.Code == http.StatusNotFound {
				t.Fatalf("iteration %d: successful registration was not reflected consistently: "+
					"registered=%v documented=%v route_status=%d",
					i, registered, documented, rec.Code)
			}
		case errors.Is(registerErr, ErrRegistrationClosed):
			if registered || documented || rec.Code != http.StatusNotFound {
				t.Fatalf("iteration %d: rejected registration leaked: "+
					"registered=%v documented=%v route_status=%d",
					i, registered, documented, rec.Code)
			}
		default:
			t.Fatalf("iteration %d: Register returned unexpected error: %v", i, registerErr)
		}
	}
}

func TestStepRegistryRegistrationIsAtomicWithFreeze(t *testing.T) {
	step := newStepRegistry("test", "default", bug03Noop)
	optionEntered := make(chan struct{})
	releaseOption := make(chan struct{})
	registerPanic := make(chan any, 1)

	go func() {
		defer func() { registerPanic <- recover() }()
		step.Register(bug03Noop, func(*MiddlewareConfig) {
			close(optionEntered)
			<-releaseOption
		})
	}()
	<-optionEntered

	step.freeze()
	close(releaseOption)
	if got := <-registerPanic; got == nil {
		t.Fatal("registration that reached the commit point after freeze did not panic")
	}
	if got := len(step.middlewares); got != 0 {
		t.Fatalf("middleware count = %d, want the post-freeze registration rejected", got)
	}
	if !step.frozen.Load() {
		t.Fatal("step was not frozen after the registration completed")
	}
}

func TestOpenAPIMiddlewareRegistrationClosesWithHandler(t *testing.T) {
	srv := bug03Server()
	_ = srv.Handler()

	for _, tc := range []struct {
		name string
		step *OpenAPIStepRegistry
	}{
		{"Auth", srv.Pipeline.OpenAPI.Auth},
		{"Generate", srv.Pipeline.OpenAPI.Generate},
		{"Response", srv.Pipeline.OpenAPI.Response},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("OpenAPI %s middleware registration after Handler was accepted", tc.name)
				}
			}()
			tc.step.Register(func(_ *OpenAPIContext, next func() error) error {
				return next()
			})
		})
	}
}

func TestOtherRouteContributorsCloseWithHandler(t *testing.T) {
	srv := bug03Server()
	_ = srv.Handler()

	for _, tc := range []struct {
		name string
		call func()
	}{
		{"Action", func() { srv.Action(ActionConfig{}) }},
		{"AllowPublic", func() { srv.AllowPublic() }},
		{"RealtimeDoc", func() { srv.RealtimeDoc(AsyncAPIConfig{}) }},
		{"EnableGlobalSearch", func() { srv.EnableGlobalSearch() }},
		{"SetStorage", func() { srv.SetStorage(nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s after Handler was accepted", tc.name)
				}
			}()
			tc.call()
		})
	}

	if err := srv.RegisterRollup(Rollup{}); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("RegisterRollup after Handler error = %v, want ErrRegistrationClosed", err)
	}
}
