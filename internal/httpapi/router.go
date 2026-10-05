package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	DB           *pgxpool.Pool
	WebOrigin    string
	OrgID        string // the single organization the MVP serves
	CookieSecure bool
	Files        FileOpener   // reads submission_files.storage_key for the review panel
	Portal       http.Handler // public applicant API, mounted at /api/v1/portal
	Links        http.Handler // form links and invitations, mounted at /api/v1/admin/links
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Logger, middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(s.cors)

	r.Get("/healthz", s.health)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		if s.Portal != nil {
			r.Mount("/portal", s.Portal)
		}

		r.Post("/auth/login", s.login)
		r.Post("/auth/logout", s.logout)
		r.With(s.requireUser).Get("/auth/me", s.me)

		r.Route("/admin", func(r chi.Router) {
			r.Use(s.requireUser)
			r.Route("/forms", s.formRoutes)
			if s.Links != nil {
				r.Mount("/links", s.Links)
			}
			s.adminRoutes(r)
		})
	})
	return r
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "db": "down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "up"})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.WebOrigin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type, Authorization", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
