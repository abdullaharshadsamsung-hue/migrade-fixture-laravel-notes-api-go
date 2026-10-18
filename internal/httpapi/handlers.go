// Package httpapi exposes the notes JSON REST API (migrated from Laravel's
// App\Http\Controllers\NoteController).
//
// MIGRATION_NOTE: this file previously held a second, duplicate
// implementation of the notes API (RegisterRoutes, Note, FileStore, ...)
// that redeclared the identifiers in router.go and broke the build. The
// single implementation now lives in router.go, which is wired into the
// server via httpapi.RegisterRoutes in cmd/server/router.go.
package httpapi