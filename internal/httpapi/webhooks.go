package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/gabrielventodev/formflow/api/internal/webhooks"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Outgoing webhooks: owners and admins register endpoints that receive a
// signed POST when submissions change status (see internal/webhooks).

const maxWebhooks = 10

func (s *Server) webhookRoutes(r chi.Router) {
	r.Get("/", s.listWebhooks)
	r.Post("/", s.createWebhook)
	r.Route("/{hookID}", func(r chi.Router) {
		r.Patch("/", s.updateWebhook)
		r.Delete("/", s.deleteWebhook)
		r.Post("/rotate-secret", s.rotateWebhookSecret)
		r.Post("/test", s.testWebhook)
		r.Get("/deliveries", s.listDeliveries)
		r.Get("/deliveries/{deliveryID}", s.getDelivery)
		r.Post("/deliveries/{deliveryID}/retry", s.retryDelivery)
	})
}

func (s *Server) allowInsecureWebhooks() bool { return s.Webhooks != nil && s.Webhooks.AllowInsecure }

type webhookOut struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	Events      []string  `json:"events"`
	IncludeData bool      `json:"include_data"`
	Active      bool      `json:"active"`
	SecretHint  string    `json:"secret_hint"`
	CreatedAt   time.Time `json:"created_at"`
	// Secret is only returned on create and rotate.
	Secret string `json:"secret,omitempty"`
	// Health of recent deliveries.
	LastDelivery *deliverySummary `json:"last_delivery"`
	Failing      int              `json:"failing"` // failed or retrying in the last 7 days
}

type deliverySummary struct {
	Status string    `json:"status"`
	Event  string    `json:"event"`
	At     time.Time `json:"at"`
}

const webhookSelect = `
	SELECT h.id, h.url, h.description, h.events, h.include_data, h.active, h.secret, h.created_at,
	       ld.status, ld.event, ld.created_at,
	       (SELECT count(*) FROM webhook_deliveries d WHERE d.webhook_id = h.id
	          AND d.created_at > now() - interval '7 days'
	          AND (d.status = 'failed' OR (d.status = 'pending' AND d.attempts > 0)))
	FROM webhooks h
	LEFT JOIN LATERAL (
		SELECT status, event, created_at FROM webhook_deliveries d
		WHERE d.webhook_id = h.id ORDER BY created_at DESC LIMIT 1
	) ld ON true`

func scanWebhook(row pgx.Row) (webhookOut, error) {
	var (
		h                 webhookOut
		secret            string
		ldStatus, ldEvent *string
		ldAt              *time.Time
	)
	err := row.Scan(&h.ID, &h.URL, &h.Description, &h.Events, &h.IncludeData, &h.Active, &secret, &h.CreatedAt,
		&ldStatus, &ldEvent, &ldAt, &h.Failing)
	if err != nil {
		return h, err
	}
	h.SecretHint = secretHint(secret)
	if ldStatus != nil {
		h.LastDelivery = &deliverySummary{Status: *ldStatus, Event: *ldEvent, At: *ldAt}
	}
	return h, nil
}

func secretHint(secret string) string {
	if len(secret) < 10 {
		return "…"
	}
	return secret[:10] + "…" + secret[len(secret)-4:]
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	rows, err := s.DB.Query(r.Context(), webhookSelect+` WHERE h.organization_id = $1 ORDER BY h.created_at`, id.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	out := []webhookOut{}
	for rows.Next() {
		h, err := scanWebhook(rows)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out, "events": webhooks.Events})
}

func (s *Server) loadWebhook(ctx context.Context, q pgx.Tx, orgID, hookID string) (webhookOut, error) {
	return scanWebhook(q.QueryRow(ctx, webhookSelect+` WHERE h.organization_id = $1 AND h.id::text = $2`, orgID, hookID))
}

type webhookInput struct {
	URL         *string   `json:"url"`
	Description *string   `json:"description"`
	Events      *[]string `json:"events"`
	IncludeData *bool     `json:"include_data"`
	Active      *bool     `json:"active"`
}

// clean validates the fields present and records the changes in meta.
func (s *Server) cleanWebhookInput(in *webhookInput, meta map[string]any) error {
	if in.URL != nil {
		u, err := webhooks.ValidateURL(*in.URL, s.allowInsecureWebhooks())
		if err != nil {
			return badRequest(capitalize(err.Error()))
		}
		in.URL = &u
		meta["url"] = u
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if utf8.RuneCountInString(d) > 200 {
			return badRequest("La descripción admite hasta 200 caracteres")
		}
		in.Description = &d
		meta["description"] = d
	}
	if in.Events != nil {
		seen := map[string]bool{}
		events := []string{}
		for _, e := range *in.Events {
			if !webhooks.ValidEvent(e) {
				return badRequest("Evento desconocido: " + e)
			}
			if !seen[e] {
				seen[e] = true
				events = append(events, e)
			}
		}
		if len(events) == 0 {
			return badRequest("Elige al menos un evento")
		}
		in.Events = &events
		meta["events"] = events
	}
	if in.IncludeData != nil {
		meta["include_data"] = *in.IncludeData
	}
	if in.Active != nil {
		meta["active"] = *in.Active
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in webhookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	if in.URL == nil {
		writeError(w, http.StatusBadRequest, "Indica la URL del endpoint", nil)
		return
	}
	if in.Events == nil {
		all := append([]string(nil), webhooks.Events...)
		in.Events = &all
	}
	meta := map[string]any{}
	if err := s.cleanWebhookInput(&in, meta); err != nil {
		s.respondWebhook(w, r, webhookOut{}, err)
		return
	}
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	secret := webhooks.NewSecret()
	var out webhookOut
	err := pgx.BeginFunc(r.Context(), s.DB, func(tx pgx.Tx) error {
		// Serialize creates per organization so the limit holds.
		if _, err := tx.Exec(r.Context(), `SELECT 1 FROM organizations WHERE id = $1 FOR UPDATE`, id.OrgID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM webhooks WHERE organization_id = $1`, id.OrgID).Scan(&n); err != nil {
			return err
		}
		if n >= maxWebhooks {
			return badRequest("Puedes registrar hasta 10 webhooks")
		}
		var hookID string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO webhooks (organization_id, url, description, secret, events, include_data, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			id.OrgID, *in.URL, desc, secret, *in.Events, in.IncludeData != nil && *in.IncludeData, id.UserID,
		).Scan(&hookID); err != nil {
			return err
		}
		meta["webhook_id"] = hookID
		if err := insertOrgAudit(r.Context(), tx, id.OrgID, id.UserID, "webhook.created", meta); err != nil {
			return err
		}
		var err error
		out, err = s.loadWebhook(r.Context(), tx, id.OrgID, hookID)
		return err
	})
	out.Secret = secret
	if err == nil {
		writeJSON(w, http.StatusCreated, out)
		return
	}
	s.respondWebhook(w, r, out, err)
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	hookID := chi.URLParam(r, "hookID")
	var in webhookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	meta := map[string]any{"webhook_id": hookID}
	if err := s.cleanWebhookInput(&in, meta); err != nil {
		s.respondWebhook(w, r, webhookOut{}, err)
		return
	}
	var out webhookOut
	err := pgx.BeginFunc(r.Context(), s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `
			UPDATE webhooks SET
				url = COALESCE($3, url),
				description = COALESCE($4, description),
				events = COALESCE($5, events),
				include_data = COALESCE($6, include_data),
				active = COALESCE($7, active),
				updated_at = now()
			WHERE organization_id = $1 AND id::text = $2`,
			id.OrgID, hookID, in.URL, in.Description, in.Events, in.IncludeData, in.Active)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		if out, err = s.loadWebhook(r.Context(), tx, id.OrgID, hookID); err != nil || len(meta) == 1 {
			return err
		}
		if _, ok := meta["url"]; !ok {
			meta["url"] = out.URL
		}
		return insertOrgAudit(r.Context(), tx, id.OrgID, id.UserID, "webhook.updated", meta)
	})
	s.respondWebhook(w, r, out, err)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	hookID := chi.URLParam(r, "hookID")
	err := pgx.BeginFunc(r.Context(), s.DB, func(tx pgx.Tx) error {
		var url string
		err := tx.QueryRow(r.Context(), `
			DELETE FROM webhooks WHERE organization_id = $1 AND id::text = $2 RETURNING url`, id.OrgID, hookID).Scan(&url)
		if err != nil {
			return err
		}
		return insertOrgAudit(r.Context(), tx, id.OrgID, id.UserID, "webhook.deleted", map[string]any{"webhook_id": hookID, "url": url})
	})
	if err != nil {
		s.respondWebhook(w, r, webhookOut{}, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	hookID := chi.URLParam(r, "hookID")
	secret := webhooks.NewSecret()
	var out webhookOut
	err := pgx.BeginFunc(r.Context(), s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `
			UPDATE webhooks SET secret = $3, updated_at = now() WHERE organization_id = $1 AND id::text = $2`,
			id.OrgID, hookID, secret)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		if out, err = s.loadWebhook(r.Context(), tx, id.OrgID, hookID); err != nil {
			return err
		}
		return insertOrgAudit(r.Context(), tx, id.OrgID, id.UserID, "webhook.secret_rotated", map[string]any{"webhook_id": hookID, "url": out.URL})
	})
	out.Secret = secret
	s.respondWebhook(w, r, out, err)
}

// testWebhook queues a "ping" for the endpoint, active or not subscribed alike.
func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	hookID := chi.URLParam(r, "hookID")
	var deliveryID string
	err := pgx.BeginFunc(r.Context(), s.DB, func(tx pgx.Tx) error {
		var active bool
		if err := tx.QueryRow(r.Context(), `SELECT active FROM webhooks WHERE organization_id = $1 AND id::text = $2`,
			id.OrgID, hookID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return badRequest("Activa el webhook para enviarle una prueba")
		}
		var err error
		deliveryID, err = webhooks.EnqueuePing(r.Context(), tx, hookID)
		return err
	})
	if err != nil {
		s.respondWebhook(w, r, webhookOut{}, err)
		return
	}
	s.Webhooks.Kick()
	writeJSON(w, http.StatusAccepted, map[string]any{"delivery_id": deliveryID})
}

type deliveryOut struct {
	ID             string          `json:"id"`
	Event          string          `json:"event"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	LastStatusCode *int            `json:"last_status_code"`
	LastError      string          `json:"last_error"`
	CreatedAt      time.Time       `json:"created_at"`
	LastAttemptAt  *time.Time      `json:"last_attempt_at"`
	NextAttemptAt  *time.Time      `json:"next_attempt_at"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

const deliveryColumns = `d.id, d.event, d.status, d.attempts, d.last_status_code, d.last_error, d.created_at,
	d.last_attempt_at, CASE WHEN d.status = 'pending' THEN d.next_attempt_at END`

func scanDelivery(row pgx.Row, withPayload bool) (deliveryOut, error) {
	var d deliveryOut
	dest := []any{&d.ID, &d.Event, &d.Status, &d.Attempts, &d.LastStatusCode, &d.LastError, &d.CreatedAt, &d.LastAttemptAt, &d.NextAttemptAt}
	if withPayload {
		dest = append(dest, &d.Payload)
	}
	return d, row.Scan(dest...)
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.Query(r.Context(), `
		SELECT `+deliveryColumns+`
		FROM webhook_deliveries d JOIN webhooks h ON h.id = d.webhook_id
		WHERE h.organization_id = $1 AND h.id::text = $2
		ORDER BY d.created_at DESC LIMIT $3`, id.OrgID, chi.URLParam(r, "hookID"), limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	out := []deliveryOut{}
	for rows.Next() {
		d, err := scanDelivery(rows, false)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	d, err := scanDelivery(s.DB.QueryRow(r.Context(), `
		SELECT `+deliveryColumns+`, d.payload
		FROM webhook_deliveries d JOIN webhooks h ON h.id = d.webhook_id
		WHERE h.organization_id = $1 AND h.id::text = $2 AND d.id::text = $3`,
		id.OrgID, chi.URLParam(r, "hookID"), chi.URLParam(r, "deliveryID")), true)
	if err != nil {
		s.respondWebhook(w, r, webhookOut{}, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// retryDelivery sends a failed delivery again, with a fresh set of attempts.
func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	tag, err := s.DB.Exec(r.Context(), `
		UPDATE webhook_deliveries d SET status = 'pending', attempts = 0, next_attempt_at = now()
		FROM webhooks h
		WHERE h.id = d.webhook_id AND h.organization_id = $1 AND h.id::text = $2 AND d.id::text = $3
		  AND d.status = 'failed'`,
		id.OrgID, chi.URLParam(r, "hookID"), chi.URLParam(r, "deliveryID"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusConflict, "Solo se pueden reintentar entregas fallidas", nil)
		return
	}
	s.Webhooks.Kick()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) respondWebhook(w http.ResponseWriter, r *http.Request, out webhookOut, err error) {
	var bad badRequest
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, out)
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, string(bad), nil)
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "Webhook no encontrado", nil)
	default:
		s.serverError(w, r, err)
	}
}
