// Package portal is the public API applicants use to fill a form without an account.
//
// An applicant opens a form link (/f/{linkToken}), leaves an email and gets a private access token
// (a magic link, /s/{accessToken}). Only the token's sha256 is stored. Every other call sends the
// token as "Authorization: Bearer <token>" so it never shows up in request logs.
package portal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/face"
	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/gabrielventodev/formflow/api/internal/storage"
	"github.com/gabrielventodev/formflow/api/internal/webhooks"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	DB    *pgxpool.Pool
	Store storage.Store
	Mail  mailer.Mailer
	// WebURL is the public URL of the Next.js app, used to build magic links in emails.
	WebURL string
	// MaxUploadMB caps any single upload, even if a field allows more.
	MaxUploadMB float64
	// OrgID scopes the admin link routes to the MVP's single organization.
	OrgID string
	// Webhooks is woken after a submission queues webhook deliveries (nil is fine).
	Webhooks *webhooks.Worker
	// Face checks liveness fields (nil = liveness fields answer 503).
	Face *face.Client
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/links/{token}", h.getLink)
	r.Post("/links/{token}/start", h.start)
	r.Post("/resume", h.resume)

	r.Group(func(r chi.Router) {
		r.Use(h.auth)
		r.Get("/submission", h.getSubmission)
		r.Put("/submission/data", h.saveData)
		r.Post("/submission/validate", h.validate)
		r.Post("/submission/submit", h.submit)
		r.Post("/submission/files", h.uploadFile)
		r.Get("/submission/files/{id}", h.downloadFile)
		r.Delete("/submission/files/{id}", h.deleteFile)
		r.Post("/submission/liveness", h.startLiveness)
		r.Post("/submission/liveness/{id}", h.finishLiveness)
	})
	return r
}

// ---- links ----

type linkInfo struct {
	ID           string
	Kind         string
	InviteeEmail *string
	ExpiresAt    *time.Time
	FormID       string
	OrgID        string
	Title        string
	Description  string
	FormStatus   string
	VersionID    *string
	Schema       json.RawMessage
}

func (h *Handler) loadLink(ctx context.Context, token string) (*linkInfo, error) {
	var l linkInfo
	err := h.DB.QueryRow(ctx, `
		SELECT l.id, l.kind, l.invitee_email, l.expires_at,
		       f.id, f.organization_id, f.title, f.description, f.status, v.id, v.schema
		FROM form_links l
		JOIN forms f ON f.id = l.form_id
		LEFT JOIN form_versions v ON v.id = f.current_version_id
		WHERE l.token = $1`, token).
		Scan(&l.ID, &l.Kind, &l.InviteeEmail, &l.ExpiresAt,
			&l.FormID, &l.OrgID, &l.Title, &l.Description, &l.FormStatus, &l.VersionID, &l.Schema)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// linkProblem says why a link can't be used, or "" if it can.
func linkProblem(l *linkInfo) string {
	switch {
	case l.ExpiresAt != nil && l.ExpiresAt.Before(time.Now()):
		return "Este enlace expiró. Pide uno nuevo a quien te lo envió."
	case l.FormStatus != "published" || l.VersionID == nil:
		return "Este formulario no está disponible."
	}
	return ""
}

func (h *Handler) getLink(w http.ResponseWriter, r *http.Request) {
	l, err := h.loadLink(r.Context(), chi.URLParam(r, "token"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Enlace no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if msg := linkProblem(l); msg != "" {
		writeError(w, http.StatusGone, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"form":         map[string]any{"title": l.Title, "description": l.Description},
		"kind":         l.Kind,
		"inviteeEmail": l.InviteeEmail,
		"schema":       l.Schema,
	})
}

type startRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// start creates a draft submission for the link and returns its access token.
// Opening an invitation again resumes the same submission with a fresh token.
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := h.loadLink(ctx, chi.URLParam(r, "token"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Enlace no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if msg := linkProblem(l); msg != "" {
		writeError(w, http.StatusGone, msg)
		return
	}
	var req startRequest
	if err := decode(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud no válida.")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.Name = strings.TrimSpace(req.Name)
	if l.Kind == "invite" && l.InviteeEmail != nil && *l.InviteeEmail != "" {
		req.Email = *l.InviteeEmail
	}
	if !validEmail(req.Email) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Revisa tus datos.", "errors": map[string]string{"email": "Ingresa un email válido."}})
		return
	}
	if len(req.Name) > 200 {
		req.Name = req.Name[:200]
	}

	token, hash := newToken()
	var subID string
	if l.Kind == "invite" {
		err = h.DB.QueryRow(ctx, `
			UPDATE submissions SET access_token_hash = $2, updated_at = now()
			WHERE form_link_id = $1
			RETURNING id`, l.ID, hash).Scan(&subID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			serverError(w, r, err)
			return
		}
	}
	if subID == "" {
		err = pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				INSERT INTO submissions (organization_id, form_id, form_version_id, form_link_id,
				                         applicant_email, applicant_name, access_token_hash)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				RETURNING id`, l.OrgID, l.FormID, *l.VersionID, l.ID, req.Email, req.Name, hash).Scan(&subID); err != nil {
				return err
			}
			return audit(ctx, tx, subID, req.Email, "submission.created", nil, strPtr("draft"), map[string]any{"link_kind": l.Kind})
		})
		if err != nil {
			serverError(w, r, err)
			return
		}
	}

	h.sendMail(r, mailer.Message{
		To:      req.Email,
		Subject: "Tu enlace para continuar: " + l.Title,
		Text: fmt.Sprintf("Hola%s,\n\nGuarda este enlace para continuar con \"%s\" cuando quieras. Tus respuestas se guardan solas.\n\n%s\n\nNo lo compartas: cualquiera con el enlace puede ver y editar tu solicitud.\n",
			greetingName(req.Name), l.Title, h.magicLink(token)),
	})
	writeJSON(w, http.StatusCreated, map[string]any{"accessToken": token, "submissionId": subID})
}

type resumeRequest struct {
	Email string `json:"email"`
}

// resume emails fresh magic links for every open submission of an email address.
// It always answers 202 so it can't be used to find out who applied.
func (h *Handler) resume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req resumeRequest
	if err := decode(r, &req, 1<<12); err != nil || !validEmail(strings.TrimSpace(req.Email)) {
		writeError(w, http.StatusBadRequest, "Ingresa un email válido.")
		return
	}
	email := strings.TrimSpace(req.Email)
	rows, err := h.DB.Query(ctx, `
		SELECT s.id, f.title FROM submissions s JOIN forms f ON f.id = s.form_id
		WHERE lower(s.applicant_email) = lower($1) AND s.status IN ('draft', 'changes_requested')
		ORDER BY s.updated_at DESC LIMIT 10`, email)
	if err != nil {
		serverError(w, r, err)
		return
	}
	type open struct{ id, title string }
	var subs []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.title); err != nil {
			rows.Close()
			serverError(w, r, err)
			return
		}
		subs = append(subs, o)
	}
	rows.Close()

	if len(subs) > 0 {
		var b strings.Builder
		b.WriteString("Hola,\n\nEstos son los enlaces para continuar tus solicitudes. Los enlaces anteriores dejan de funcionar.\n\n")
		for _, s := range subs {
			token, hash := newToken()
			if _, err := h.DB.Exec(ctx, `UPDATE submissions SET access_token_hash = $2 WHERE id = $1`, s.id, hash); err != nil {
				serverError(w, r, err)
				return
			}
			fmt.Fprintf(&b, "- %s: %s\n", s.title, h.magicLink(token))
		}
		h.sendMail(r, mailer.Message{To: email, Subject: "Tus enlaces para continuar", Text: b.String()})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "ok"})
}

// ---- helpers ----

func (h *Handler) magicLink(token string) string {
	return strings.TrimRight(h.WebURL, "/") + "/s/" + token
}

// sendMail never blocks the request; failures are logged.
func (h *Handler) sendMail(r *http.Request, m mailer.Message) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Mail.Send(ctx, m); err != nil {
			slog.Error("send email", "to", m.To, "err", err)
		}
	}()
}

func newToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func audit(ctx context.Context, q execer, subID, actor, action string, from, to *string, meta map[string]any) error {
	if meta == nil {
		meta = map[string]any{}
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_events (submission_id, actor_type, actor_id, action, from_status, to_status, metadata)
		VALUES ($1, 'applicant', $2, $3, $4, $5, $6)`, subID, actor, action, from, to, meta)
	return err
}

func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && strings.Contains(s[strings.LastIndex(s, "@")+1:], ".")
}

func greetingName(n string) string {
	if n == "" {
		return ""
	}
	return " " + n
}

func strPtr(s string) *string { return &s }

func decode(r *http.Request, v any, limit int64) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit)).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("portal", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "Ocurrió un error. Intenta de nuevo.")
}
