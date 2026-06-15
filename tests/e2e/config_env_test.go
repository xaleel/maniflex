package e2e_test

import (
	"testing"
	"time"

	"maniflex"
)

func TestConfigFromEnv_WithPrefix(t *testing.T) {
	t.Setenv("SVC_PORT", "9090")
	t.Setenv("SVC_DB_WRITE_URL", "postgres://user:pass@primary/mydb")
	t.Setenv("SVC_DB_READ_URL", "postgres://user:pass@replica/mydb")
	t.Setenv("SVC_QUERY_TIMEOUT_MS", "5000")
	t.Setenv("SVC_SHUTDOWN_TIMEOUT_S", "45")
	t.Setenv("SVC_SERVICE_NAME", "my-service")
	t.Setenv("SVC_HEALTH_CHECK_DB", "true")

	cfg := maniflex.ConfigFromEnv("SVC")

	if cfg.Port != 9090 {
		t.Errorf("Port: want 9090, got %d", cfg.Port)
	}
	if cfg.DBWriteURL != "postgres://user:pass@primary/mydb" {
		t.Errorf("DBWriteURL: got %q", cfg.DBWriteURL)
	}
	if cfg.DBReadURL != "postgres://user:pass@replica/mydb" {
		t.Errorf("DBReadURL: got %q", cfg.DBReadURL)
	}
	if cfg.QueryTimeout != 5000*time.Millisecond {
		t.Errorf("QueryTimeout: want 5s, got %v", cfg.QueryTimeout)
	}
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Errorf("ShutdownTimeout: want 45s, got %v", cfg.ShutdownTimeout)
	}
	if cfg.ServiceName != "my-service" {
		t.Errorf("ServiceName: got %q", cfg.ServiceName)
	}
	if !cfg.HealthCheckDB {
		t.Error("HealthCheckDB: want true")
	}
}

func TestConfigFromEnv_NoPrefix(t *testing.T) {
	t.Setenv("PORT", "3000")
	t.Setenv("DB_WRITE_URL", "sqlite:./data.db")
	t.Setenv("SERVICE_NAME", "bare-service")
	t.Setenv("HEALTH_CHECK_DB", "1")

	cfg := maniflex.ConfigFromEnv("")

	if cfg.Port != 3000 {
		t.Errorf("Port: want 3000, got %d", cfg.Port)
	}
	if cfg.DBWriteURL != "sqlite:./data.db" {
		t.Errorf("DBWriteURL: got %q", cfg.DBWriteURL)
	}
	if cfg.ServiceName != "bare-service" {
		t.Errorf("ServiceName: got %q", cfg.ServiceName)
	}
	if !cfg.HealthCheckDB {
		t.Error("HealthCheckDB: want true")
	}
}

func TestConfigFromEnv_Unset(t *testing.T) {
	// All env vars absent — should return zero Config, no panics.
	cfg := maniflex.ConfigFromEnv("NOTSET_XYZ")

	if cfg.Port != 0 {
		t.Errorf("Port: want 0, got %d", cfg.Port)
	}
	if cfg.DBWriteURL != "" {
		t.Errorf("DBWriteURL: want empty, got %q", cfg.DBWriteURL)
	}
	if cfg.QueryTimeout != 0 {
		t.Errorf("QueryTimeout: want 0, got %v", cfg.QueryTimeout)
	}
	if cfg.HealthCheckDB {
		t.Error("HealthCheckDB: want false")
	}
}

func TestConfigFromEnv_BoolVariants(t *testing.T) {
	for _, v := range []string{"true", "1", "yes", "on", "TRUE", "YES"} {
		t.Setenv("B_HEALTH_CHECK_DB", v)
		cfg := maniflex.ConfigFromEnv("B")
		if !cfg.HealthCheckDB {
			t.Errorf("HEALTH_CHECK_DB=%q: want true", v)
		}
	}
	for _, v := range []string{"false", "0", "no", "off", ""} {
		t.Setenv("B_HEALTH_CHECK_DB", v)
		cfg := maniflex.ConfigFromEnv("B")
		if cfg.HealthCheckDB {
			t.Errorf("HEALTH_CHECK_DB=%q: want false", v)
		}
	}
}
