package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"migrated-app/internal/httpapi"
)

// buildRouter creates the application's chi router and mounts every
// migrated route-owning package on it.
func buildRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// Notes JSON API (migrated from Laravel routes/api.php + NoteController).
	httpapi.RegisterRoutes(r)

	return r
}