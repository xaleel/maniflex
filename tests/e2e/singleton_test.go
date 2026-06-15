package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// AppConfig is a ModelConfig.Singleton fixture: one config row the admin edits
// and clients read at launch. No mfx:"required" fields (singletons auto-provision
// their row from column defaults).
type AppConfig struct {
	maniflex.BaseModel
	MaintenanceMode bool   `json:"maintenance_mode" db:"maintenance_mode" mfx:"default:false"`
	MinAppVersion   string `json:"min_app_version"  db:"min_app_version"  mfx:"default:1.0.0"`
	Banner          string `json:"banner"           db:"banner"`
}

// singletonServer stands up a server with AppConfig mounted as a singleton at
// /config.
func singletonServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{AppConfig{}, maniflex.ModelConfig{Singleton: true, TableName: "config"}},
	})
}

func TestSingleton(t *testing.T) {
	t.Parallel()

	t.Run("get_provisions_row_with_defaults", func(t *testing.T) {
		t.Parallel()
		srv := singletonServer(t)

		data := srv.GET("/config").AssertStatus(http.StatusOK).Data()
		if data["id"] != maniflex.SingletonID {
			t.Errorf("id: got %v, want %q", data["id"], maniflex.SingletonID)
		}
		if data["maintenance_mode"] != false {
			t.Errorf("maintenance_mode: got %v, want false", data["maintenance_mode"])
		}
		if data["min_app_version"] != "1.0.0" {
			t.Errorf("min_app_version: got %v, want 1.0.0 (column default)", data["min_app_version"])
		}
		if data["banner"] != "" {
			t.Errorf("banner: got %v, want empty string", data["banner"])
		}
	})

	t.Run("patch_updates_the_single_row", func(t *testing.T) {
		t.Parallel()
		srv := singletonServer(t)

		updated := srv.PATCH("/config", map[string]any{
			"maintenance_mode": true,
			"banner":           "Down for maintenance",
		}).AssertStatus(http.StatusOK).Data()

		if updated["maintenance_mode"] != true {
			t.Errorf("maintenance_mode after patch: got %v, want true", updated["maintenance_mode"])
		}
		if updated["banner"] != "Down for maintenance" {
			t.Errorf("banner after patch: got %v", updated["banner"])
		}
		// Untouched field keeps its default.
		if updated["min_app_version"] != "1.0.0" {
			t.Errorf("min_app_version after patch: got %v, want 1.0.0", updated["min_app_version"])
		}

		// A subsequent GET reflects the persisted change and the same row id.
		got := srv.GET("/config").AssertStatus(http.StatusOK).Data()
		if got["maintenance_mode"] != true || got["banner"] != "Down for maintenance" {
			t.Errorf("get after patch did not reflect update: %v", got)
		}
		if got["id"] != maniflex.SingletonID {
			t.Errorf("id: got %v, want %q", got["id"], maniflex.SingletonID)
		}
	})

	t.Run("patch_before_any_get_provisions_then_updates", func(t *testing.T) {
		t.Parallel()
		srv := singletonServer(t)

		// First request of any kind is a PATCH: the row must be provisioned then
		// updated, not 404.
		updated := srv.PATCH("/config", map[string]any{"banner": "hello"}).
			AssertStatus(http.StatusOK).Data()
		if updated["banner"] != "hello" {
			t.Errorf("banner: got %v, want hello", updated["banner"])
		}
	})

	t.Run("provisioning_is_idempotent_across_requests", func(t *testing.T) {
		t.Parallel()
		srv := singletonServer(t)

		first := srv.GET("/config").AssertStatus(http.StatusOK).Data()
		second := srv.GET("/config").AssertStatus(http.StatusOK).Data()
		// Same fixed id on every call ⇒ there is exactly one backing row.
		if first["id"] != maniflex.SingletonID || second["id"] != maniflex.SingletonID {
			t.Errorf("ids: got %v and %v, want %q", first["id"], second["id"], maniflex.SingletonID)
		}
	})

	t.Run("collection_verbs_and_item_path_are_not_mounted", func(t *testing.T) {
		t.Parallel()
		srv := singletonServer(t)

		// No create / delete on a singleton.
		srv.POST("/config", map[string]any{"banner": "x"}).AssertStatus(http.StatusMethodNotAllowed)
		srv.DELETE("/config").AssertStatus(http.StatusMethodNotAllowed)

		// No /{id} subtree — the row is addressed without an id.
		srv.GET("/config/" + maniflex.SingletonID).AssertStatus(http.StatusNotFound)
	})

	t.Run("openapi_documents_only_get_and_patch", func(t *testing.T) {
		t.Parallel()
		srv := singletonServer(t)

		var spec map[string]any
		if err := json.Unmarshal(srv.GET("/openapi.json").Body, &spec); err != nil {
			t.Fatalf("openapi spec is not valid JSON: %v", err)
		}
		paths := spec["paths"].(map[string]any)

		cfgPath, ok := paths["/config"].(map[string]any)
		if !ok {
			t.Fatalf("spec missing /config path; got paths %v", keysOf(paths))
		}
		for _, want := range []string{"get", "patch"} {
			if _, ok := cfgPath[want]; !ok {
				t.Errorf("/config missing %s operation", want)
			}
		}
		for _, unwanted := range []string{"post", "delete"} {
			if _, ok := cfgPath[unwanted]; ok {
				t.Errorf("/config should not document %s on a singleton", unwanted)
			}
		}
		if _, ok := paths["/config/{id}"]; ok {
			t.Error("singleton must not document a /config/{id} item path")
		}
	})
}

// BadSingleton has a required field, which is incompatible with auto-provisioning.
type BadSingleton struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
}

func TestSingletonRejectsRequiredFields(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{})
	err := srv.Register(BadSingleton{}, maniflex.ModelConfig{Singleton: true})
	if err == nil {
		t.Fatal("expected Register to reject a singleton model with a required field")
	}
}
