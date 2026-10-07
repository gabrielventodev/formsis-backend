package portal

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/mailer"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// AdminRoutes manage the links that open a form: one public link to share anywhere, or
// personal invitations sent by email. Mounted at /api/v1/admin/links.
//
// TODO(auth): open until admin sessions land with the admin panel, like /admin/forms.
func (h *Handler) AdminRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.listLinks)
	r.Post("/", h.createLink)
	r.Delete("/{id}", h.deleteLink)
	return r
}

type linkOut struct {
	ID           string     `json:"id"`
	FormID       string     `json:"formId"`
	Kind         string     `json:"kind"`
	Token        string     `json:"token"`
	URL          string     `json:"url"`
	InviteeEmail *string    `json:"inviteeEmail"`
	ExpiresAt    *time.Time `json:"expiresAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	Submissions  int        `json:"submissions"`
}

func (h *Handler) formURL(token string) string {
	return strings.TrimRight(h.WebURL, "/") + "/f/" + token
}

func (h *Handler) listLinks(w http.ResponseWriter, r *http.Request) {
	formID := r.URL.Query().Get("formId")
	rows, err := h.DB.Query(r.Context(), `
		SELECT l.id, l.form_id, l.kind, l.token, l.invitee_email, l.expires_at, l.created_at,
		       (SELECT count(*) FROM submissions s WHERE s.form_link_id = l.id)
		FROM form_links l JOIN forms f ON f.id = l.form_id
		WHERE f.organization_id = $1 AND l.form_id = $2
		ORDER BY l.created_at DESC`, h.OrgID, formID)
	if err != nil {
		if isInvalidUUID(err) {
			writeError(w, http.StatusBadRequest, "Formulario no válido.")
			return
		}
		serverError(w, r, err)
		return
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (linkOut, error) {
		var l linkOut
		err := row.Scan(&l.ID, &l.FormID, &l.Kind, &l.Token, &l.InviteeEmail, &l.ExpiresAt, &l.CreatedAt, &l.Submissions)
		l.URL = h.formURL(l.Token)
		return l, err
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(links))
}

type createLinkRequest struct {
	FormID       string     `json:"formId"`
	Kind         string     `json:"kind"` // public | invite
	InviteeEmail string     `json:"inviteeEmail"`
	InviteeName  string     `json:"inviteeName"`
	Message      string     `json:"message"`
	ExpiresAt    *time.Time `json:"expiresAt"`
}

func (h *Handler) createLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createLinkRequest
	if err := decode(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud no válida.")
		return
	}
	if req.Kind == "" {
		req.Kind = "public"
	}
	req.InviteeEmail = strings.TrimSpace(req.InviteeEmail)
	switch req.Kind {
	case "public":
		req.InviteeEmail = ""
	case "invite":
		if !validEmail(req.InviteeEmail) {
			writeError(w, http.StatusBadRequest, "Ingresa un email válido para la invitación.")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "Tipo de enlace no válido.")
		return
	}

	var title, status string
	err := h.DB.QueryRow(ctx, `SELECT title, status FROM forms WHERE id = $1 AND organization_id = $2`,
		req.FormID, h.OrgID).Scan(&title, &status)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "Formulario no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if status != "published" {
		writeError(w, http.StatusConflict, "Publica el formulario antes de compartirlo.")
		return
	}

	b := make([]byte, 18)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	var invitee *string
	if req.InviteeEmail != "" {
		invitee = &req.InviteeEmail
	}
	l := linkOut{FormID: req.FormID, Kind: req.Kind, Token: token, URL: h.formURL(token), InviteeEmail: invitee, ExpiresAt: req.ExpiresAt}
	if err := h.DB.QueryRow(ctx, `
		INSERT INTO form_links (form_id, token, kind, invitee_email, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		req.FormID, token, req.Kind, invitee, req.ExpiresAt).Scan(&l.ID, &l.CreatedAt); err != nil {
		serverError(w, r, err)
		return
	}

	if req.Kind == "invite" {
		text := fmt.Sprintf("Hola%s,\n\nTe invitamos a completar \"%s\".", greetingName(strings.TrimSpace(req.InviteeName)), title)
		if msg := strings.TrimSpace(req.Message); msg != "" {
			text += "\n\n" + msg
		}
		text += "\n\nEntra aquí para empezar. Puedes guardar y continuar cuando quieras:\n" + l.URL + "\n"
		if req.ExpiresAt != nil {
			text += fmt.Sprintf("\nEl enlace vence el %s.\n", req.ExpiresAt.Format("02-01-2006"))
		}
		h.sendMail(r, mailer.Message{To: req.InviteeEmail, Subject: "Te invitamos a completar: " + title, Text: text})
	}
	writeJSON(w, http.StatusCreated, l)
}

// deleteLink disables a link. Submissions already started keep working through their own magic links.
func (h *Handler) deleteLink(w http.ResponseWriter, r *http.Request) {
	tag, err := h.DB.Exec(r.Context(), `
		DELETE FROM form_links l USING forms f
		WHERE l.id = $1 AND f.id = l.form_id AND f.organization_id = $2`, chi.URLParam(r, "id"), h.OrgID)
	if isInvalidUUID(err) || (err == nil && tag.RowsAffected() == 0) {
		writeError(w, http.StatusNotFound, "Enlace no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
