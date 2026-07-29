package maniflex

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type bug05MigrationSpy struct {
	DBAdapter
	calls atomic.Int32
}

func (s *bug05MigrationSpy) AutoMigrate(context.Context, RegistryAccessor) error {
	s.calls.Add(1)
	return nil
}

type Bug05ValidModel struct {
	BaseModel
	Name string `json:"name"`
}

type Bug05LateModel struct {
	BaseModel
	Value string `json:"value"`
}

type Bug05RelationTarget struct {
	BaseModel
	Name string `json:"name"`
}

type Bug05DanglingRelation struct {
	BaseModel
	Targets []Bug05RelationTarget `json:"targets" mfx:"relation,through:NoSuchBug05Junction"`
}

func TestMigrateOnlyValidatesBeforeCallingAdapters(t *testing.T) {
	t.Run("broken relation", func(t *testing.T) {
		spy := &bug05MigrationSpy{}
		srv := New(Config{DB: spy})
		if err := srv.Register(Bug05DanglingRelation{}, Bug05RelationTarget{}); err != nil {
			t.Fatalf("Register: %v", err)
		}

		err := srv.MigrateOnly(context.Background())
		if err == nil || !strings.Contains(err.Error(), "NoSuchBug05Junction") {
			t.Fatalf("MigrateOnly error = %v, want missing-junction validation error", err)
		}
		if got := spy.calls.Load(); got != 0 {
			t.Fatalf("AutoMigrate calls = %d, want 0 after validation failure", got)
		}
	})

	t.Run("ineffective middleware", func(t *testing.T) {
		spy := &bug05MigrationSpy{}
		srv := New(Config{DB: spy})
		if err := srv.Register(Bug05ValidModel{}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		srv.Pipeline.DB.Register(
			func(_ *ServerContext, next func() error) error { return next() },
			ForOperation(OpAction),
			WithName("bug05-never-runs"),
		)

		err := srv.MigrateOnly(context.Background())
		if err == nil || !strings.Contains(err.Error(), "bug05-never-runs") {
			t.Fatalf("MigrateOnly error = %v, want ineffective-middleware validation error", err)
		}
		if got := spy.calls.Load(); got != 0 {
			t.Fatalf("AutoMigrate calls = %d, want 0 after validation failure", got)
		}
	})

	t.Run("strict router configuration", func(t *testing.T) {
		spy := &bug05MigrationSpy{}
		srv := New(Config{
			DB:     spy,
			Strict: true,
			FilesConfig: FilesConfig{
				MountEndpoints: true,
			},
		})
		if err := srv.Register(Bug05ValidModel{}); err != nil {
			t.Fatalf("Register: %v", err)
		}

		err := srv.MigrateOnly(context.Background())
		if err == nil || !strings.Contains(err.Error(), "BeforeMiddlewares") {
			t.Fatalf("MigrateOnly error = %v, want strict files validation error", err)
		}
		if got := spy.calls.Load(); got != 0 {
			t.Fatalf("AutoMigrate calls = %d, want 0 after validation failure", got)
		}
	})
}

func TestMigrateOnlyValidatesThenMigratesAndSeals(t *testing.T) {
	spy := &bug05MigrationSpy{}
	srv := New(Config{DB: spy})
	if err := srv.Register(Bug05ValidModel{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("MigrateOnly: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("AutoMigrate calls = %d, want 1", got)
	}
	if err := srv.Register(Bug05LateModel{}); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("Register after MigrateOnly error = %v, want ErrRegistrationClosed", err)
	}
}
