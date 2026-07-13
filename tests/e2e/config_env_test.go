package e2e_test

import (
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// mustEnvConfig reads the config and fails the test if a variable was rejected.
func mustEnvConfig(t *testing.T, prefix string) maniflex.Config {
	t.Helper()
	cfg, err := maniflex.ConfigFromEnv(prefix)
	if err != nil {
		t.Fatalf("ConfigFromEnv(%q): %v", prefix, err)
	}
	return cfg
}

func TestConfigFromEnv_WithPrefix(t *testing.T) {
	t.Setenv("SVC_PORT", "9090")
	t.Setenv("SVC_DB_WRITE_URL", "postgres://user:pass@primary/mydb")
	t.Setenv("SVC_DB_READ_URL", "postgres://user:pass@replica/mydb")
	t.Setenv("SVC_QUERY_TIMEOUT_MS", "5000")
	t.Setenv("SVC_SHUTDOWN_TIMEOUT_S", "45")
	t.Setenv("SVC_SERVICE_NAME", "my-service")
	t.Setenv("SVC_HEALTH_CHECK_DB", "true")

	cfg := mustEnvConfig(t, "SVC")

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

	cfg := mustEnvConfig(t, "")

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
	// Every variable absent — a zero Config and no error. Unset is not malformed:
	// the zero values are exactly what ApplyDefaults exists to fill in.
	cfg := mustEnvConfig(t, "NOTSET_XYZ")

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
		if !mustEnvConfig(t, "B").HealthCheckDB {
			t.Errorf("HEALTH_CHECK_DB=%q: want true", v)
		}
	}
	for _, v := range []string{"false", "0", "no", "off", ""} {
		t.Setenv("B_HEALTH_CHECK_DB", v)
		if mustEnvConfig(t, "B").HealthCheckDB {
			t.Errorf("HEALTH_CHECK_DB=%q: want false", v)
		}
	}
}

// A variable that was set but unreadable used to be dropped on the floor: the
// server booted on the default port, with no query timeout, looking healthy —
// and the only evidence was the behaviour you did not get (DX-5).
func TestConfigFromEnv_MalformedValueIsAnError(t *testing.T) {
	cases := []struct {
		name    string
		field   string // the variable the operator meant to set
		value   string
		wantErr string // the message must name the variable and the value
	}{
		{"letter O for zero in the port", "PORT", "808O", `MAL_PORT: invalid integer "808O"`},
		{"port out of range", "PORT", "70000", "MAL_PORT: port out of range: 70000"},
		{"negative port", "PORT", "-1", "MAL_PORT: port out of range: -1"},
		{"non-numeric timeout", "QUERY_TIMEOUT_MS", "abc", `MAL_QUERY_TIMEOUT_MS: invalid integer "abc"`},
		{"zero timeout", "QUERY_TIMEOUT_MS", "0", "MAL_QUERY_TIMEOUT_MS: must be a positive number of milliseconds"},
		{"negative shutdown", "SHUTDOWN_TIMEOUT_S", "-5", "MAL_SHUTDOWN_TIMEOUT_S: must be a positive number of seconds"},
		{"typo'd bool", "HEALTH_CHECK_DB", "ture", `MAL_HEALTH_CHECK_DB: invalid boolean "ture"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAL_"+tc.field, tc.value)

			_, err := maniflex.ConfigFromEnv("MAL")
			if err == nil {
				t.Fatalf("%s=%q was accepted: the caller gets a Config that ignores it, "+
					"and a server that boots on the defaults looking healthy", tc.field, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the error must name the variable and the value it could not read\n"+
					"got:  %v\nwant substring: %s", err, tc.wantErr)
			}
		})
	}
}

// Two typos should cost one deploy to find, not two.
func TestConfigFromEnv_ReportsEveryBadVariable(t *testing.T) {
	t.Setenv("MULTI_PORT", "80 80")
	t.Setenv("MULTI_QUERY_TIMEOUT_MS", "5s")
	t.Setenv("MULTI_HEALTH_CHECK_DB", "maybe")

	_, err := maniflex.ConfigFromEnv("MULTI")
	if err == nil {
		t.Fatal("three malformed variables were all accepted")
	}
	for _, want := range []string{"MULTI_PORT", "MULTI_QUERY_TIMEOUT_MS", "MULTI_HEALTH_CHECK_DB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s is missing from the error — fixing one typo would only reveal the next\ngot: %v", want, err)
		}
	}
}

// A rejected variable must not leak a half-parsed value, and must not stop the
// readable variables around it from being read.
func TestConfigFromEnv_MalformedValueLeavesFieldZero(t *testing.T) {
	t.Setenv("ZERO_PORT", "70000")
	t.Setenv("ZERO_SERVICE_NAME", "still-read")

	cfg, err := maniflex.ConfigFromEnv("ZERO")
	if err == nil {
		t.Fatal("the out-of-range port was accepted")
	}
	if cfg.Port != 0 {
		t.Errorf("Port: want 0 (rejected), got %d", cfg.Port)
	}
	if cfg.ServiceName != "still-read" {
		t.Errorf("ServiceName: want %q, got %q", "still-read", cfg.ServiceName)
	}
}
