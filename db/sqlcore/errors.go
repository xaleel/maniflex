package sqlcore

// ErrorNormalizer converts a raw driver error into a *maniflex.ErrConstraint when it
// represents a known constraint violation (unique or foreign-key), so the DB
// step can return 409 Conflict instead of an opaque 500. Errors that are not
// constraint violations must be returned unchanged.
//
// Constraint errors are driver-specific, so sqlcore does not handle them
// itself: lib/pq surfaces a typed *pq.Error with SQLSTATE codes, while
// modernc.org/sqlite reports them only as message strings. Each driver package
// (db/postgres, db/sqlite) implements its own ErrorNormalizer and registers it
// via Adapter.SetErrorNormalizer, keeping this package free of any driver
// dependency.
//
// table is the offending model's table name, used to populate ErrConstraint.Table.
type ErrorNormalizer func(err error, table string) error

// SetErrorNormalizer registers the driver-specific constraint-error normalizer.
// Driver packages call this from their Open constructor. When no normalizer is
// registered, driver errors from Create/Update pass through unchanged.
func (a *Adapter) SetErrorNormalizer(fn ErrorNormalizer) {
	a.errNormalizer = fn
}

// normalizeErr applies fn when both fn and err are non-nil; otherwise it
// returns err unchanged.
func normalizeErr(fn ErrorNormalizer, err error, table string) error {
	if err == nil || fn == nil {
		return err
	}
	return fn(err, table)
}
