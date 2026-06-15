package maniflex

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ConfigFromEnv builds a Config by reading standard environment variables.
//
// If prefix is non-empty (e.g. "ORDERS"), each variable is prefixed with
// an underscore separator: ORDERS_PORT, ORDERS_DB_WRITE_URL, etc.
// If prefix is empty the variables are read without a prefix: PORT,
// DB_WRITE_URL, etc.
//
// Variables read:
//
//	PREFIX_PORT               → Config.Port            (integer)
//	PREFIX_DB_WRITE_URL       → Config.DBWriteURL      (string)
//	PREFIX_DB_READ_URL        → Config.DBReadURL        (string)
//	PREFIX_QUERY_TIMEOUT_MS   → Config.QueryTimeout     (milliseconds)
//	PREFIX_SHUTDOWN_TIMEOUT_S → Config.ShutdownTimeout  (seconds)
//	PREFIX_SERVICE_NAME       → Config.ServiceName      (string)
//	PREFIX_HEALTH_CHECK_DB    → Config.HealthCheckDB    (bool: "true"/"1"/"yes")
//
// Unset or empty variables are silently ignored; Config.ApplyDefaults() is
// NOT called — callers may customise further before passing to maniflex.New().
func ConfigFromEnv(prefix string) Config {
	env := func(suffix string) string {
		key := suffix
		if prefix != "" {
			key = prefix + "_" + suffix
		}
		return strings.TrimSpace(os.Getenv(key))
	}

	var cfg Config

	if v := env("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}
	cfg.DBWriteURL = env("DB_WRITE_URL")
	cfg.DBReadURL = env("DB_READ_URL")
	if v := env("QUERY_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.QueryTimeout = time.Duration(n) * time.Millisecond
		}
	}
	if v := env("SHUTDOWN_TIMEOUT_S"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.ShutdownTimeout = time.Duration(n) * time.Second
		}
	}
	cfg.ServiceName = env("SERVICE_NAME")
	if v := env("HEALTH_CHECK_DB"); v != "" {
		cfg.HealthCheckDB = parseBool(v)
	}

	return cfg
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
