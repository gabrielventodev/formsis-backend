package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/gabrielventodev/formflow/api/internal/schema"
	"github.com/gabrielventodev/formflow/api/internal/webhooks"
	"github.com/jackc/pgx/v5"
)

type submission struct {
	ID          string
	OrgID       string
	FormTitle   string
	FormDesc    string
	Email       string
	Name        string
	Status      string
	Data        map[string]any
	RawSchema   json.RawMessage
	Schema      schema.Schema
	SubmittedAt *time.Time
	UpdatedAt   time.Time
}

type ctxKey struct{}

func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "Falta el enlace de acceso.")
			return
		}
		s, err := h.loadSubmission(r.Context(), h.DB, hashToken(token), false)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "Este enlace ya no es válido. Pide uno nuevo con tu email.")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	})
}

func current(r *http.Request) *submission { return r.Context().Value(ctxKey{}).(*submission) }

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (h *Handler) loadSubmission(ctx context.Context, q queryer, tokenHash string, forUpdate bool) (*submission, error) {
	sql := `
		SELECT s.id, s.organization_id, f.title, f.description, s.applicant_email, s.applicant_name,
		       s.status, s.data, v.schema, s.submitted_at, s.updated_at
		FROM submissions s
		JOIN forms f ON f.id = s.form_id
		JOIN form_versions v ON v.id = s.form_version_id
		WHERE s.access_token_hash = $1`
	if forUpdate {
		sql += " FOR UPDATE OF s"
	}
	var s submission
	err := q.QueryRow(ctx, sql, tokenHash).Scan(&s.ID, &s.OrgID, &s.FormTitle, &s.FormDesc, &s.Email, &s.Name,
		&s.Status, &s.Data, &s.RawSchema, &s.SubmittedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if s.Data == nil {
		s.Data = map[string]any{}
	}
	if s.Schema, err = schema.Parse(s.RawSchema); err != nil {
		return nil, err
	}
	return &s, nil
}

type fileOut struct {
	ID         string    `json:"id"`
	FieldKey   string    `json:"fieldKey"`
	Filename   string    `json:"filename"`
	MimeType   string    `json:"mimeType"`
	SizeBytes  int64     `json:"sizeBytes"`
	UploadedAt time.Time `json:"uploadedAt"`
}

type commentOut struct {
	ID        string    `json:"id"`
	FieldKey  *string   `json:"fieldKey"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

func (h *Handler) listFiles(ctx context.Context, subID string) ([]fileOut, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT id, field_key, filename, mime_type, size_bytes, uploaded_at
		FROM submission_files WHERE submission_id = $1 ORDER BY uploaded_at`, subID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fileOut, error) {
		var f fileOut
		err := row.Scan(&f.ID, &f.FieldKey, &f.Filename, &f.MimeType, &f.SizeBytes, &f.UploadedAt)
		return f, err
	})
}

// openComments are the reviewer's unresolved notes; they decide what the applicant may change.
func (h *Handler) openComments(ctx context.Context, subID string) ([]commentOut, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT id, field_key, body, created_at FROM review_comments
		WHERE submission_id = $1 AND resolved_at IS NULL ORDER BY created_at`, subID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (commentOut, error) {
		var c commentOut
		err := row.Scan(&c.ID, &c.FieldKey, &c.Body, &c.CreatedAt)
		return c, err
	})
}

// editable returns which top-level fields the applicant may change.
// nil with ok=true means everything; ok=false means the submission is read-only.
func editable(status string, comments []commentOut) (keys map[string]bool, ok bool) {
	switch status {
	case "draft":
		return nil, true
	case "changes_requested":
		keys = map[string]bool{}
		for _, c := range comments {
			if c.FieldKey == nil || *c.FieldKey == "" {
				return nil, true
			}
			keys[rootKey(*c.FieldKey)] = true
		}
		if len(keys) == 0 {
			return nil, true
		}
		return keys, true
	}
	return nil, false
}

func rootKey(path string) string {
	k, _, _ := strings.Cut(path, ".")
	return k
}

func (h *Handler) getSubmission(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	files, err := h.listFiles(ctx, s.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	liveness, err := h.listLiveness(ctx, s.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	var comments []commentOut
	if s.Status == "changes_requested" {
		if comments, err = h.openComments(ctx, s.ID); err != nil {
			serverError(w, r, err)
			return
		}
	}
	keys, ok := editable(s.Status, comments)
	var editableFields []string
	for k := range keys {
		editableFields = append(editableFields, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             s.ID,
		"form":           map[string]any{"title": s.FormTitle, "description": s.FormDesc},
		"applicant":      map[string]any{"email": s.Email, "name": s.Name},
		"status":         s.Status,
		"data":           s.Data,
		"schema":         s.RawSchema,
		"files":          nonNil(files),
		"liveness":       nonNil(liveness),
		"comments":       nonNil(comments),
		"canEdit":        ok,
		"editableFields": editableFields, // null = all fields
		"submittedAt":    s.SubmittedAt,
		"updatedAt":      s.UpdatedAt,
	})
}

type saveRequest struct {
	Data map[string]any `json:"data"`
}

// saveData is the autosave endpoint: it stores answers without validating them.
func (h *Handler) saveData(w http.ResponseWriter, r *http.Request) {
	var req saveRequest
	if err := decode(r, &req, 2<<20); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud no válida.")
		return
	}
	savedAt, err := h.storeData(r.Context(), current(r), req.Data)
	if errors.Is(err, errReadOnly) {
		writeError(w, http.StatusConflict, "Esta solicitud ya fue enviada y no se puede editar.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"savedAt": savedAt})
}

var errReadOnly = errors.New("submission is read-only")

// storeData saves answers, keeping only schema fields and, after a review, only the flagged ones.
func (h *Handler) storeData(ctx context.Context, s *submission, in map[string]any) (time.Time, error) {
	var comments []commentOut
	var err error
	if s.Status == "changes_requested" {
		if comments, err = h.openComments(ctx, s.ID); err != nil {
			return time.Time{}, err
		}
	}
	keys, ok := editable(s.Status, comments)
	if !ok {
		return time.Time{}, errReadOnly
	}
	data := s.Schema.Clean(in)
	if keys != nil {
		merged := map[string]any{}
		for k, v := range s.Data {
			merged[k] = v
		}
		for k := range keys {
			if v, ok := data[k]; ok {
				merged[k] = v
			} else {
				delete(merged, k)
			}
		}
		data = merged
	}
	var updatedAt time.Time
	err = h.DB.QueryRow(ctx, `
		UPDATE submissions SET data = $2, updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'changes_requested') RETURNING updated_at`,
		s.ID, data).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, errReadOnly
	}
	s.Data = data
	return updatedAt, err
}

func (h *Handler) fileCounts(ctx context.Context, subID string) (schema.FileCounts, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT field_key, count(*) FROM submission_files WHERE submission_id = $1 GROUP BY field_key
		UNION ALL
		SELECT field_key, count(*) FROM liveness_checks
		WHERE submission_id = $1 AND decision IN ('pass', 'review') GROUP BY field_key`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := schema.FileCounts{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		counts[k] += n
	}
	return counts, rows.Err()
}

type validateRequest struct {
	Section string `json:"section"` // empty = whole form
}

// validate checks the saved answers (one step or all) with the server rules.
func (h *Handler) validate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	var req validateRequest
	_ = decode(r, &req, 1<<12)
	counts, err := h.fileCounts(ctx, s.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	var errs schema.Errors
	if req.Section != "" {
		errs = s.Schema.ValidateSection(req.Section, s.Data, counts)
	} else {
		errs = s.Schema.ValidateAnswers(s.Data, counts)
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": len(errs) == 0, "errors": errs})
}

type submitRequest struct {
	Data map[string]any `json:"data"` // optional final save
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req submitRequest
	_ = decode(r, &req, 2<<20)
	if req.Data != nil {
		if _, err := h.storeData(ctx, current(r), req.Data); err != nil && !errors.Is(err, errReadOnly) {
			serverError(w, r, err)
			return
		}
	}

	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var (
		s       *submission
		errs    schema.Errors
		from    string
		problem string
	)
	err := pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		var err error
		if s, err = h.loadSubmission(ctx, tx, hashToken(token), true); err != nil {
			return err
		}
		from = s.Status
		if from != "draft" && from != "changes_requested" {
			problem = "Esta solicitud ya fue enviada."
			return nil
		}
		counts, err := h.fileCounts(ctx, s.ID)
		if err != nil {
			return err
		}
		if errs = s.Schema.ValidateAnswers(s.Data, counts); len(errs) > 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE submissions SET status = 'submitted', submitted_at = now(), updated_at = now()
			WHERE id = $1`, s.ID); err != nil {
			return err
		}
		action := "submission.submitted"
		if from == "changes_requested" {
			action = "submission.resubmitted"
			if _, err := tx.Exec(ctx, `
				UPDATE review_comments SET resolved_at = now()
				WHERE submission_id = $1 AND resolved_at IS NULL`, s.ID); err != nil {
				return err
			}
		}
		if err := audit(ctx, tx, s.ID, s.Email, action, &from, strPtr("submitted"), nil); err != nil {
			return err
		}
		return webhooks.EnqueueSubmission(ctx, tx, webhooks.SubmissionEvent{
			Type: webhooks.EventSubmitted, SubmissionID: s.ID, FromStatus: from, WebURL: h.WebURL,
			Extra: map[string]any{"resubmitted": from == "changes_requested"},
		})
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	if problem != "" {
		writeError(w, http.StatusConflict, problem)
		return
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Faltan datos o hay respuestas por corregir.", "errors": errs})
		return
	}

	h.Webhooks.Kick()
	subject := "Recibimos tu solicitud: " + s.FormTitle
	if from == "changes_requested" {
		subject = "Recibimos tus correcciones: " + s.FormTitle
	}
	h.sendMail(r, mailer.Message{
		To:      s.Email,
		Subject: subject,
		Text: fmt.Sprintf("Hola%s,\n\nRecibimos tu solicitud para \"%s\". Nuestro equipo la revisará y te avisaremos por este medio.\n\nPuedes ver lo que enviaste aquí:\n%s\n",
			greetingName(s.Name), s.FormTitle, h.magicLink(token)),
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "submitted"})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
