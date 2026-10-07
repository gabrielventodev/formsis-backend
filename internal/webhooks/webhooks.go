// Package webhooks sends signed HTTP notifications to the organization's
// endpoints when a submission changes status.
//
// Deliveries are queued in the same transaction as the change (so a rolled
// back change never notifies) and a background Worker posts them, retrying
// with exponential backoff.
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Events an endpoint can subscribe to. "ping" is only sent by the "send test" action.
const (
	EventSubmitted        = "submission.submitted"
	EventInReview         = "submission.in_review"
	EventStepApproved     = "submission.step_approved"
	EventChangesRequested = "submission.changes_requested"
	EventApproved         = "submission.approved"
	EventRejected         = "submission.rejected"
	EventPing             = "ping"
)

var Events = []string{EventSubmitted, EventInReview, EventStepApproved, EventChangesRequested, EventApproved, EventRejected}

func ValidEvent(e string) bool {
	for _, v := range Events {
		if v == e {
			return true
		}
	}
	return false
}

// StatusEvent maps a submission's new status to its event.
func StatusEvent(status string) string { return "submission." + status }

// NewSecret returns a random signing secret.
func NewSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "whsec_" + hex.EncodeToString(b)
}

// Sign returns the Formsis-Signature header value: t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<body>">.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature header against the body, rejecting ones older
// than tolerance. It documents the scheme receivers implement.
func Verify(secret, header string, body []byte, tolerance time.Duration, now time.Time) bool {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return false
	}
	if d := now.Sub(time.Unix(n, 0)); d > tolerance || d < -tolerance {
		return false
	}
	want := Sign(secret, time.Unix(n, 0), body)
	return hmac.Equal([]byte(want), []byte("t="+ts+",v1="+sig))
}

// Payload is the JSON body every delivery carries.
type Payload struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt time.Time      `json:"created_at"`
	Data      map[string]any `json:"data"`
}

func newEventID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}

// SubmissionEvent describes a status change to notify.
type SubmissionEvent struct {
	Type         string
	SubmissionID string
	FromStatus   string
	WebURL       string         // public web URL, for the admin link
	Extra        map[string]any // merged into data (e.g. step names)
}

// EnqueueSubmission queues ev for every active endpoint of the submission's
// organization subscribed to ev.Type. Call it inside the transaction that
// makes the change.
func EnqueueSubmission(ctx context.Context, tx pgx.Tx, ev SubmissionEvent) error {
	var (
		orgID, formID, formTitle, email, name, status string
		version, step                                 int
		submittedAt, decidedAt                        *time.Time
		answers                                       json.RawMessage
	)
	err := tx.QueryRow(ctx, `
		SELECT s.organization_id, s.form_id, f.title, v.version_number, s.applicant_email, s.applicant_name,
		       s.status::text, s.approval_step, s.submitted_at, s.decided_at, s.data
		FROM submissions s
		JOIN forms f ON f.id = s.form_id
		JOIN form_versions v ON v.id = s.form_version_id
		WHERE s.id = $1`, ev.SubmissionID,
	).Scan(&orgID, &formID, &formTitle, &version, &email, &name, &status, &step, &submittedAt, &decidedAt, &answers)
	if err != nil {
		return fmt.Errorf("webhook event: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, include_data FROM webhooks
		WHERE organization_id = $1 AND active AND $2 = ANY(events)`, orgID, ev.Type)
	if err != nil {
		return err
	}
	type hook struct {
		id          string
		includeData bool
	}
	var hooks []hook
	for rows.Next() {
		var h hook
		if err := rows.Scan(&h.id, &h.includeData); err != nil {
			rows.Close()
			return err
		}
		hooks = append(hooks, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(hooks) == 0 {
		return err
	}

	data := map[string]any{
		"submission": map[string]any{
			"id":            ev.SubmissionID,
			"status":        status,
			"approval_step": step,
			"submitted_at":  submittedAt,
			"decided_at":    decidedAt,
			"admin_url":     strings.TrimRight(ev.WebURL, "/") + "/admin/envios/" + ev.SubmissionID,
		},
		"form":        map[string]any{"id": formID, "title": formTitle, "version": version},
		"applicant":   map[string]any{"email": email, "name": name},
		"from_status": ev.FromStatus,
	}
	for k, v := range ev.Extra {
		data[k] = v
	}
	p := Payload{ID: newEventID(), Type: ev.Type, CreatedAt: time.Now().UTC(), Data: data}
	for _, h := range hooks {
		p.Data = data
		if h.includeData {
			withAnswers := make(map[string]any, len(data)+1)
			for k, v := range data {
				withAnswers[k] = v
			}
			withAnswers["answers"] = answers
			p.Data = withAnswers
		}
		if err := insertDelivery(ctx, tx, h.id, p); err != nil {
			return err
		}
	}
	return nil
}

// EnqueuePing queues a test event for one endpoint and returns the delivery id.
func EnqueuePing(ctx context.Context, tx pgx.Tx, hookID string) (string, error) {
	p := Payload{ID: newEventID(), Type: EventPing, CreatedAt: time.Now().UTC(), Data: map[string]any{
		"message": "Prueba de webhook desde Formsis",
	}}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (webhook_id, event, payload) VALUES ($1, $2, $3) RETURNING id`,
		hookID, p.Type, b).Scan(&id)
	return id, err
}

func insertDelivery(ctx context.Context, tx pgx.Tx, hookID string, p Payload) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO webhook_deliveries (webhook_id, event, payload) VALUES ($1, $2, $3)`, hookID, p.Type, b)
	return err
}
