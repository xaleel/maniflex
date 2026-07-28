package maniflex

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

type ProductionWidget struct {
	BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
}

func productionConfig() Config {
	return Config{
		Strict:             true,
		DisableAutoMigrate: true,
		QueryTimeout:       5 * time.Second,
	}
}

func productionNoopAuth(_ *ServerContext, next func() error) error {
	return next()
}

func TestValidateProduction_ReportsDangerousDefaultsTogether(t *testing.T) {
	t.Parallel()
	srv := New(Config{})
	srv.MustRegister(ProductionWidget{})

	err := srv.ValidateProduction()
	if err == nil {
		t.Fatal("dangerous defaults unexpectedly passed production validation")
	}
	msg := err.Error()
	for _, want := range []string{
		"Config.Strict",
		"Config.QueryTimeout",
		"DisableAutoMigrate",
		`model "ProductionWidget"`,
		"create",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("production error missing %q:\n%s", want, msg)
		}
	}
}

func TestValidateProduction_ProtectedGeneratedRoutesPass(t *testing.T) {
	t.Parallel()
	srv := New(productionConfig())
	srv.MustRegister(
		ProductionWidget{},
		ModelConfig{ExportEnabled: true, Versioned: true, AggregateEnabled: true},
	)
	srv.Pipeline.Auth.Register(
		productionNoopAuth,
		ForOperation(OpCreate, OpRead, OpUpdate, OpDelete, OpList),
	)

	if err := srv.ValidateProduction(); err != nil {
		t.Fatalf("protected generated routes failed production validation: %v", err)
	}
}

func TestValidateProduction_AllowPublicFillsOnlyDeclaredScope(t *testing.T) {
	t.Parallel()
	srv := New(productionConfig())
	srv.MustRegister(ProductionWidget{})
	srv.Pipeline.Auth.Register(
		productionNoopAuth,
		ForOperation(OpRead, OpUpdate, OpDelete, OpList),
	)

	err := srv.ValidateProduction()
	if err == nil || !strings.Contains(err.Error(), "create") {
		t.Fatalf("uncovered create should fail, got %v", err)
	}

	srv.AllowPublic(
		ForModel("ProductionWidget"),
		ForOperation(OpCreate),
	)
	if err := srv.ValidateProduction(); err != nil {
		t.Fatalf("explicitly public create was not accepted: %v", err)
	}
}

func TestValidateProduction_RejectsDisabledQueryLimit(t *testing.T) {
	t.Parallel()
	cfg := productionConfig()
	cfg.QueryLimits.MaxIncludes = -1
	srv := New(cfg)
	srv.MustRegister(ProductionWidget{})
	srv.Pipeline.Auth.Register(productionNoopAuth)

	err := srv.ValidateProduction()
	if err == nil || !strings.Contains(err.Error(), "MaxIncludes") {
		t.Fatalf("disabled query limit should be reported, got %v", err)
	}
}

func TestValidateProduction_RejectsPerModelDisabledQueryLimit(t *testing.T) {
	t.Parallel()
	srv := New(productionConfig())
	srv.MustRegister(
		ProductionWidget{},
		ModelConfig{QueryLimits: QueryLimits{MaxIncludes: -1}},
	)
	srv.Pipeline.Auth.Register(productionNoopAuth)

	err := srv.ValidateProduction()
	if err == nil ||
		!strings.Contains(err.Error(), `model "ProductionWidget" QueryLimits`) ||
		!strings.Contains(err.Error(), "MaxIncludes") {
		t.Fatalf("per-model disabled query limit should be reported, got %v", err)
	}
}

func TestValidateProduction_RejectsUnboundedGlobalSearch(t *testing.T) {
	t.Parallel()
	srv := New(productionConfig())
	srv.EnableGlobalSearch(GlobalSearchConfig{
		MaxLimit:    -1,
		AllowPublic: true,
	})

	err := srv.ValidateProduction()
	if err == nil || !strings.Contains(err.Error(), "GlobalSearchConfig.MaxLimit") {
		t.Fatalf("unbounded global search should be reported, got %v", err)
	}
}

func TestValidateProduction_AuxiliaryRoutesNeedAccessDecision(t *testing.T) {
	t.Parallel()
	cfg := productionConfig()
	cfg.FilesConfig.MountEndpoints = true
	srv := New(cfg)
	srv.Action(ActionConfig{
		Method: http.MethodPost,
		Path:   "/reindex",
		Handler: func(*ServerContext) error {
			return nil
		},
	})
	srv.EnableGlobalSearch()

	err := srv.ValidateProduction()
	if err == nil {
		t.Fatal("unprotected auxiliary routes unexpectedly passed")
	}
	msg := err.Error()
	for _, want := range []string{"/files", "POST /reindex", "global search"} {
		if !strings.Contains(msg, want) {
			t.Errorf("production error missing %q:\n%s", want, msg)
		}
	}
}

func TestValidateProduction_ExplicitPublicAuxiliaryRoutesPass(t *testing.T) {
	t.Parallel()
	cfg := productionConfig()
	cfg.Documentation.Public = true
	cfg.FilesConfig.MountEndpoints = true
	cfg.FilesConfig.AllowPublic = true
	srv := New(cfg)
	srv.Action(ActionConfig{
		Method:      http.MethodGet,
		Path:        "/catalog",
		Handler:     func(*ServerContext) error { return nil },
		AllowPublic: true,
	})
	srv.Action(ActionConfig{
		Method:           http.MethodPost,
		Path:             "/internal/reindex",
		Handler:          func(*ServerContext) error { return nil },
		AccessControlled: true,
	})
	srv.EnableGlobalSearch(GlobalSearchConfig{AllowPublic: true})

	if err := srv.ValidateProduction(); err != nil {
		t.Fatalf("explicit auxiliary decisions failed production validation: %v", err)
	}
	if err := srv.validateRegistry(); err != nil {
		t.Fatalf("FilesConfig.AllowPublic must also satisfy Strict startup validation: %v", err)
	}
}

func TestValidateProduction_GlobalHTTPAccessAssertion(t *testing.T) {
	t.Parallel()
	cfg := productionConfig()
	cfg.HTTPAccessControlled = true
	srv := New(cfg)
	srv.MustRegister(ProductionWidget{})

	err := srv.ValidateProduction()
	if err == nil || !strings.Contains(err.Error(), "HTTPMiddlewares is empty") {
		t.Fatalf("empty asserted HTTP access policy should fail, got %v", err)
	}

	cfg.HTTPMiddlewares = []HTTPMiddleware{
		func(next http.Handler) http.Handler { return next },
	}
	srv = New(cfg)
	srv.MustRegister(ProductionWidget{})
	if err := srv.ValidateProduction(); err != nil {
		t.Fatalf("asserted router-level access policy failed: %v", err)
	}
}

func TestAllowPublic_PanicsAfterRouterIsBuilt(t *testing.T) {
	t.Parallel()
	srv := New(productionConfig())
	srv.MustRegister(ProductionWidget{})
	_ = srv.Handler()

	defer func() {
		if recover() == nil {
			t.Fatal("AllowPublic after Handler should panic")
		}
	}()
	srv.AllowPublic()
}
