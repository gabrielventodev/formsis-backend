package portal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/face"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Liveness from a phone. On a computer the portal asks for a handoff and shows its link as a QR.
// The link's token only lets the phone run the liveness check of that one field; the computer
// polls the handoff until an attempt finishes. Opening a new handoff closes the previous one.
const handoffTTL = 10 * time.Minute

type handoff struct {
	ID       string
	FieldKey string
}

// id is the handoff id for SQL, NULL when the request comes from the applicant's own session.
func (ho *handoff) id() *string {
	if ho == nil {
		return nil
	}
	return &ho.ID
}

type handoffKey struct{}

func currentHandoff(r *http.Request) *handoff {
	ho, _ := r.Context().Value(handoffKey{}).(*handoff)
	return ho
}

// handoffAuth authenticates the phone with the token from the QR and loads its submission.
func (h *Handler) handoffAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "Falta el código de verificación.")
			return
		}
		ctx := r.Context()
		var ho handoff
		var subID string
		err := h.DB.QueryRow(ctx, `
			SELECT id, field_key, submission_id FROM liveness_handoffs
			WHERE token_hash = $1 AND expires_at > now()`, hashToken(token)).Scan(&ho.ID, &ho.FieldKey, &subID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "Este código ya no es válido. Genera uno nuevo desde el computador.")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		s, err := h.loadSubmissionWhere(ctx, h.DB, "s.id = $1", subID, false)
		if err != nil {
			serverError(w, r, err)
			return
		}
		ctx = context.WithValue(ctx, ctxKey{}, s)
		ctx = context.WithValue(ctx, handoffKey{}, &ho)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// createHandoff (computer): POST /submission/liveness/handoff {fieldKey}.
func (h *Handler) createHandoff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	var req livenessStartRequest
	if err := decode(r, &req, 1<<12); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud no válida.")
		return
	}
	msg, status, err := h.livenessField(ctx, s, req.FieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	used, err := h.attemptsUsed(ctx, s.ID, req.FieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if used >= livenessMaxAttempts {
		writeError(w, http.StatusTooManyRequests, "Llegaste al máximo de intentos. Escríbenos para que revisemos tu caso.")
		return
	}
	token, hash := newToken()
	var id string
	var expiresAt time.Time
	err = pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE liveness_handoffs SET expires_at = now()
			WHERE submission_id = $1 AND field_key = $2 AND expires_at > now()`, s.ID, req.FieldKey); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO liveness_handoffs (submission_id, field_key, token_hash, expires_at)
			VALUES ($1, $2, $3, now() + $4::interval) RETURNING id, expires_at`,
			s.ID, req.FieldKey, hash, fmt.Sprintf("%d seconds", int(handoffTTL.Seconds()))).Scan(&id, &expiresAt)
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        id,
		"url":       strings.TrimRight(h.WebURL, "/") + "/v/" + token,
		"expiresAt": expiresAt,
	})
}

// handoffStatus (computer): GET /submission/liveness/handoff/{id}, polled while the QR is shown.
func (h *Handler) handoffStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	var fieldKey string
	var expiresAt time.Time
	var openedAt *time.Time
	var checking bool
	err := h.DB.QueryRow(ctx, `
		SELECT field_key, expires_at, opened_at,
		       EXISTS (SELECT 1 FROM liveness_checks c
		               WHERE c.handoff_id = ho.id AND c.decision IS NULL AND c.expires_at > now())
		FROM liveness_handoffs ho WHERE id = $1 AND submission_id = $2`, chi.URLParam(r, "id"), s.ID).
		Scan(&fieldKey, &expiresAt, &openedAt, &checking)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "Código no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	rows, err := h.DB.Query(ctx, `
		SELECT id, field_key, decision, reasons, completed_at FROM liveness_checks
		WHERE handoff_id = $1 AND decision IS NOT NULL ORDER BY created_at`, chi.URLParam(r, "id"))
	if err != nil {
		serverError(w, r, err)
		return
	}
	attempts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (livenessOut, error) {
		var l livenessOut
		err := row.Scan(&l.ID, &l.FieldKey, &l.Decision, &l.Reasons, &l.CompletedAt)
		return l, err
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	used, err := h.attemptsUsed(ctx, s.ID, fieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"expiresAt":    expiresAt,
		"expired":      !time.Now().Before(expiresAt),
		"openedAt":     openedAt,
		"checking":     checking,
		"attempts":     attempts,
		"attemptsLeft": max(livenessMaxAttempts-used, 0),
	})
}

// getHandoff (phone): GET /handoff. Tells the phone what it is verifying, and nothing about the applicant.
func (h *Handler) getHandoff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	ho := currentHandoff(r)
	if _, err := h.DB.Exec(ctx, `UPDATE liveness_handoffs SET opened_at = COALESCE(opened_at, now()) WHERE id = $1`, ho.ID); err != nil {
		serverError(w, r, err)
		return
	}
	f := s.Schema.Find(ho.FieldKey)
	if f == nil {
		writeError(w, http.StatusConflict, "Este campo ya no existe en el formulario.")
		return
	}
	var completed bool
	if err := h.DB.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM liveness_checks WHERE submission_id = $1 AND field_key = $2
		               AND decision = ANY($3))`, s.ID, ho.FieldKey, []string{face.Pass, face.Review}).Scan(&completed); err != nil {
		serverError(w, r, err)
		return
	}
	used, err := h.attemptsUsed(ctx, s.ID, ho.FieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"form":         map[string]any{"title": s.FormTitle},
		"field":        map[string]any{"label": f.Label, "help": f.Help},
		"completed":    completed,
		"attemptsLeft": max(livenessMaxAttempts-used, 0),
	})
}
