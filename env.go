package maniflex

import (
	"errors"
	"fmt"
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
//	PREFIX_PORT               → Config.Port             (integer, 1-65535)
//	PREFIX_DB_WRITE_URL       → Config.DBWriteURL       (string)
//	PREFIX_DB_READ_URL        → Config.DBReadURL        (string)
//	PREFIX_QUERY_TIMEOUT_MS   → Config.QueryTimeout     (positive integer, milliseconds)
//	PREFIX_SHUTDOWN_TIMEOUT_S → Config.ShutdownTimeout  (positive integer, seconds)
//	PREFIX_SERVICE_NAME       → Config.ServiceName      (string)
//	PREFIX_HEALTH_CHECK_DB    → Config.HealthCheckDB    (bool: true/false, 1/0, yes/no, on/off)
//
// A variable that is unset or empty is left at its zero value, for
// Config.ApplyDefaults to fill in later. A variable that is *set but unreadable*
// is an error: it names the variable and the value it could not parse, and every
// such variable is reported at once, so two typos take one deploy to find rather
// than two. Nothing is silently ignored — a mistyped PORT should stop the process,
// not leave it listening on 8080 and looking healthy.
//
// ApplyDefaults is NOT called here; callers may customise further before passing
// the Config to maniflex.New, which applies them.
//
//	cfg, err := maniflex.ConfigFromEnv("ORDERS")
//	if err != nil {
//	    log.Fatal(err) // e.g. maniflex: ORDERS_PORT: invalid integer "808O"
//	}
func ConfigFromEnv(prefix string) (Config, error) {
	e := &envReader{prefix: prefix}

	var cfg Config
	cfg.Port = e.port("PORT")
	cfg.DBWriteURL = e.str("DB_WRITE_URL")
	cfg.DBReadURL = e.str("DB_READ_URL")
	cfg.QueryTimeout = e.duration("QUERY_TIMEOUT_MS", time.Millisecond)
	cfg.ShutdownTimeout = e.duration("SHUTDOWN_TIMEOUT_S", time.Second)
	cfg.ServiceName = e.str("SERVICE_NAME")
	cfg.HealthCheckDB = e.bool("HEALTH_CHECK_DB")

	return cfg, e.err()
}

// envReader reads prefixed variables and collects the ones that were set but
// could not be read, rather than returning at the first.
type envReader struct {
	prefix string
	errs   []error
}

// lookup returns the fully qualified variable name and its trimmed value.
func (e *envReader) lookup(suffix string) (key, val string) {
	key = suffix
	if e.prefix != "" {
		key = e.prefix + "_" + suffix
	}
	return key, strings.TrimSpace(os.Getenv(key))
}

func (e *envReader) fail(key string, format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf("maniflex: %s: %s", key, fmt.Sprintf(format, args...)))
}

func (e *envReader) err() error { return errors.Join(e.errs...) }

func (e *envReader) str(suffix string) string {
	_, val := e.lookup(suffix)
	return val
}

// port reads a TCP port. The range check is the point: a negative or oversized
// port used to pass through to http.Server and surface much later as an opaque
// listen error, far from the variable that caused it.
func (e *envReader) port(suffix string) int {
	key, val := e.lookup(suffix)
	if val == "" {
		return 0
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		e.fail(key, "invalid integer %q", val)
		return 0
	}
	if n < 1 || n > 65535 {
		e.fail(key, "port out of range: %d (want 1-65535)", n)
		return 0
	}
	return n
}

// duration reads a positive count of unit-sized ticks (milliseconds, seconds).
func (e *envReader) duration(suffix string, unit time.Duration) time.Duration {
	key, val := e.lookup(suffix)
	if val == "" {
		return 0
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		e.fail(key, "invalid integer %q", val)
		return 0
	}
	if n <= 0 {
		e.fail(key, "must be a positive number of %s, got %d", unitName(unit), n)
		return 0
	}
	return time.Duration(n) * unit
}

func (e *envReader) bool(suffix string) bool {
	key, val := e.lookup(suffix)
	if val == "" {
		return false
	}
	switch strings.ToLower(val) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	e.fail(key, "invalid boolean %q (want true/false, 1/0, yes/no, or on/off)", val)
	return false
}

func unitName(unit time.Duration) string {
	if unit == time.Millisecond {
		return "milliseconds"
	}
	return "seconds"
}
