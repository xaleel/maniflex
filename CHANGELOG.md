# Changelog

## v0.1.1 (2026-06-16)

- Fixed the server failing to start when no models are registered; the minimal app now boots and serves `/health` and an empty OpenAPI document
- `db/sqlite` and `db/postgres` now resolve `db/sqlcore` through the core module instead of a separate, non-reproducible `db/sqlcore` dependency
- Corrected Go import paths throughout the documentation to use the full `github.com/xaleel/maniflex` module prefix
- Documented that `server.Handler()` does not run AutoMigrate()

## v0.1.0 (2026-06-15)

- Release of the mostly-stable v0.1.0
- `admin` package remains experimental
