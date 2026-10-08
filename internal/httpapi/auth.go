package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/auth"
	"github.com/gabrielventodev/formsis/api/internal/ratelimit"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
)

func (s *Server) authStore() *auth.Store { return &auth.Store{DB: s.DB} }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.Email == "" || in.Password == "" {
		writeError(w, http.StatusBadRequest, "Ingresa email y contraseña", nil)
		return
	}
	emailKey := strings.ToLower(strings.TrimSpace(in.Email))
	if !s.limits().loginIP.Allow(ratelimit.ClientIP(r)) || s.limits().loginEmail.Blocked(emailKey) {
		writeError(w, http.StatusTooManyRequests, "Demasiados intentos. Espera unos minutos y vuelve a intentar.", nil)
		return
	}
	token, id, err := s.authStore().Login(r.Context(), in.Email, in.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, pgx.ErrNoRows) {
		s.limits().loginEmail.Hit(emailKey)
		writeError(w, http.StatusUnauthorized, "Email o contraseña incorrectos", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.SetCookie(w, s.sessionCookie(token, time.Now().Add(auth.SessionTTL)))
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		_ = s.authStore().Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, s.sessionCookie("", time.Unix(0, 0)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) sessionCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     auth.CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// requireUser rejects requests without a valid admin session.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(auth.CookieName)
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "Inicia sesión", nil)
			return
		}
		id, err := s.authStore().Lookup(r.Context(), c.Value)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "Tu sesión expiró", nil)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
	})
}

// requireRole allows only the listed membership roles (use after requireUser).
func requireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, _ := auth.FromContext(r.Context())
			for _, role := range roles {
				if id.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, "No tienes permiso para esta acción", nil)
		})
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "err", err, "path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()))
	writeError(w, http.StatusInternalServerError, "Error interno, intenta de nuevo", nil)
}
