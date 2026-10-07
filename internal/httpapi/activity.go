package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/auth"
)

type activityRow struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorName  string          `json:"actor_name"`
	Action     string          `json:"action"`
	FromStatus *string         `json:"from_status"`
	ToStatus   *string         `json:"to_status"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  time.Time       `json:"created_at"`
	Submission *struct {
		ID        string `json:"id"`
		Applicant string `json:"applicant"`
		FormTitle string `json:"form_title"`
	} `json:"submission"`
}

// listActivity is the organization-wide audit trail, newest first, paged by
// event id: ?before=<id>&limit=50&scope=all|submissions|team.
func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 50)
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var before *int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Cursor inválido", nil)
			return
		}
		before = &n
	}
	scope := q.Get("scope")
	switch scope {
	case "", "all", "submissions", "team":
	default:
		writeError(w, http.StatusBadRequest, "Filtro inválido", nil)
		return
	}

	rows, err := s.DB.Query(r.Context(), `
		SELECT a.id, a.actor_type, COALESCE(NULLIF(u.name, ''), u.email, ''), a.action,
		       a.from_status, a.to_status, a.metadata, a.created_at,
		       sub.id::text, COALESCE(NULLIF(sub.applicant_name, ''), sub.applicant_email, ''), COALESCE(f.title, '')
		FROM audit_events a
		LEFT JOIN submissions sub ON sub.id = a.submission_id
		LEFT JOIN forms f ON f.id = sub.form_id
		LEFT JOIN users u ON a.actor_type = 'user' AND u.id::text = a.actor_id
		WHERE COALESCE(sub.organization_id, a.organization_id) = $1
		  AND ($2::bigint IS NULL OR a.id < $2)
		  AND ($3 = '' OR $3 = 'all'
		       OR ($3 = 'submissions' AND a.submission_id IS NOT NULL)
		       OR ($3 = 'team' AND a.submission_id IS NULL))
		ORDER BY a.id DESC
		LIMIT $4`, id.OrgID, before, scope, limit+1)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	items := []activityRow{}
	for rows.Next() {
		var e activityRow
		var subID *string
		var applicant, formTitle string
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorName, &e.Action, &e.FromStatus, &e.ToStatus,
			&e.Metadata, &e.CreatedAt, &subID, &applicant, &formTitle); err != nil {
			s.serverError(w, r, err)
			return
		}
		if subID != nil {
			e.Submission = &struct {
				ID        string `json:"id"`
				Applicant string `json:"applicant"`
				FormTitle string `json:"form_title"`
			}{*subID, applicant, formTitle}
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}
	var next *int64
	if len(items) > limit {
		items = items[:limit]
		next = &items[limit-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_before": next})
}
