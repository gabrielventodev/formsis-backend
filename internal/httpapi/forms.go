package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gabrielventodev/formflow/api/internal/forms"
	"github.com/gabrielventodev/formflow/api/internal/schema"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TODO(auth): these routes are open until admin sessions land with the admin panel;
// the organization is the single default one.
func (s *Server) formRoutes(r chi.Router) {
	r.Get("/", s.listForms)
	r.Post("/", s.createForm)
	r.Route("/{formID}", func(r chi.Router) {
		r.Get("/", s.getForm)
		r.Patch("/", s.updateForm)
		r.Delete("/", s.deleteForm)
		r.Post("/publish", s.publishForm)
		r.Post("/duplicate", s.duplicateForm)
		r.Post("/archive", s.archiveForm(true))
		r.Post("/restore", s.archiveForm(false))
		r.Get("/validate", s.validateForm)
		r.Get("/versions", s.listVersions)
		r.Get("/versions/{number}", s.getVersion)
	})
}

func (s *Server) forms() *forms.Store { return &forms.Store{DB: s.DB} }

func (s *Server) listForms(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "", "draft", "published", "archived":
	default:
		writeError(w, http.StatusBadRequest, "estado inválido", nil)
		return
	}
	list, err := s.forms().List(r.Context(), s.OrgID, status)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type formInput struct {
	Title       *string          `json:"title"`
	Description *string          `json:"description"`
	Schema      *json.RawMessage `json:"schema"`
}

func (s *Server) createForm(w http.ResponseWriter, r *http.Request) {
	var in formInput
	if !readJSON(w, r, &in) {
		return
	}
	title := ""
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if title == "" {
		writeError(w, http.StatusBadRequest, "El formulario necesita un título", nil)
		return
	}
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	f, err := s.forms().Create(r.Context(), s.OrgID, title, desc)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) getForm(w http.ResponseWriter, r *http.Request) {
	f, err := s.forms().Get(r.Context(), s.OrgID, chi.URLParam(r, "formID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// updateForm saves the draft. The schema must be well-formed JSON of the right shape,
// but may still be incomplete; publishing is what requires a fully valid schema.
func (s *Server) updateForm(w http.ResponseWriter, r *http.Request) {
	var in formInput
	if !readJSON(w, r, &in) {
		return
	}
	u := forms.Update{Description: in.Description}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" {
			writeError(w, http.StatusBadRequest, "El formulario necesita un título", nil)
			return
		}
		u.Title = &t
	}
	if in.Schema != nil {
		sc, err := schema.Parse(*in.Schema)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		u.Schema = &sc
	}
	f, err := s.forms().UpdateDraft(r.Context(), s.OrgID, chi.URLParam(r, "formID"), u)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) validateForm(w http.ResponseWriter, r *http.Request) {
	f, err := s.forms().Get(r.Context(), s.OrgID, chi.URLParam(r, "formID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	sc, err := schema.Parse(f.Schema)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"problems": []schema.Problem{{Message: err.Error()}}})
		return
	}
	problems := schema.Validate(sc)
	if problems == nil {
		problems = []schema.Problem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": problems})
}

func (s *Server) publishForm(w http.ResponseWriter, r *http.Request) {
	f, err := s.forms().Publish(r.Context(), s.OrgID, chi.URLParam(r, "formID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) duplicateForm(w http.ResponseWriter, r *http.Request) {
	f, err := s.forms().Duplicate(r.Context(), s.OrgID, chi.URLParam(r, "formID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) archiveForm(archived bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := s.forms().SetArchived(r.Context(), s.OrgID, chi.URLParam(r, "formID"), archived)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, f)
	}
}

func (s *Server) deleteForm(w http.ResponseWriter, r *http.Request) {
	if err := s.forms().Delete(r.Context(), s.OrgID, chi.URLParam(r, "formID")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	list, err := s.forms().Versions(r.Context(), s.OrgID, chi.URLParam(r, "formID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "number"))
	if err != nil {
		writeError(w, http.StatusNotFound, forms.ErrNotFound.Error(), nil)
		return
	}
	v, err := s.forms().Version(r.Context(), s.OrgID, chi.URLParam(r, "formID"), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// fail maps store errors to HTTP responses.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var invalid *forms.InvalidSchemaError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusUnprocessableEntity, invalid.Error(), invalid.Problems)
	case errors.Is(err, forms.ErrNotFound), isInvalidUUID(err):
		writeError(w, http.StatusNotFound, forms.ErrNotFound.Error(), nil)
	case errors.Is(err, forms.ErrArchived), errors.Is(err, forms.ErrNoChanges), errors.Is(err, forms.ErrPublished):
		writeError(w, http.StatusConflict, err.Error(), nil)
	default:
		slog.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "error interno", nil)
	}
}

// isInvalidUUID reports Postgres rejecting a malformed id, which for callers is just "not found".
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "JSON inválido", nil)
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, msg string, problems []schema.Problem) {
	body := map[string]any{"error": msg}
	if problems != nil {
		body["problems"] = problems
	}
	writeJSON(w, status, body)
}
