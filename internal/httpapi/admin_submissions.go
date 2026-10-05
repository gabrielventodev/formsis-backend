package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// adminRoutes mounts the review panel API. All routes require a session.
func (s *Server) adminRoutes(r chi.Router) {
	r.Get("/submissions", s.listSubmissions)
	r.Get("/submissions/facets", s.submissionFacets)
	r.Get("/submissions/export.csv", s.exportSubmissions)
	r.Get("/submissions/{id}", s.getSubmission)
	r.Post("/submissions/{id}/transition", s.transitionSubmission)
	r.Post("/submissions/{id}/comments", s.addComment)
	r.Post("/submissions/{id}/comments/{commentID}/resolve", s.resolveComment)
	r.Put("/submissions/{id}/assignee", s.assignSubmission)
	r.Get("/submissions/{id}/files/{fileID}", s.downloadFile)
}

type userRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type submissionRow struct {
	ID             string     `json:"id"`
	FormID         string     `json:"form_id"`
	FormTitle      string     `json:"form_title"`
	VersionNumber  int        `json:"version_number"`
	ApplicantEmail string     `json:"applicant_email"`
	ApplicantName  string     `json:"applicant_name"`
	Status         string     `json:"status"`
	AssignedTo     *userRef   `json:"assigned_to"`
	SubmittedAt    *time.Time `json:"submitted_at"`
	DecidedAt      *time.Time `json:"decided_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const submissionSelect = `
	SELECT s.id, s.form_id, f.title, v.version_number, s.applicant_email, s.applicant_name, s.status,
	       u.id, u.name, u.email, s.submitted_at, s.decided_at, s.created_at, s.updated_at
	FROM submissions s
	JOIN forms f ON f.id = s.form_id
	JOIN form_versions v ON v.id = s.form_version_id
	LEFT JOIN users u ON u.id = s.assigned_to`

func scanSubmission(row pgx.Row) (submissionRow, error) {
	var sr submissionRow
	var uid, uname, uemail *string
	err := row.Scan(&sr.ID, &sr.FormID, &sr.FormTitle, &sr.VersionNumber, &sr.ApplicantEmail, &sr.ApplicantName,
		&sr.Status, &uid, &uname, &uemail, &sr.SubmittedAt, &sr.DecidedAt, &sr.CreatedAt, &sr.UpdatedAt)
	if uid != nil {
		sr.AssignedTo = &userRef{ID: *uid, Name: *uname, Email: *uemail}
	}
	return sr, err
}

var validStatuses = map[string]bool{
	"draft": true, "submitted": true, "in_review": true,
	"changes_requested": true, "approved": true, "rejected": true,
}

// submissionFilter turns query params into a WHERE clause shared by the list and the CSV export.
func submissionFilter(r *http.Request, id auth.Identity) (string, []any, error) {
	q := r.URL.Query()
	where := []string{"s.organization_id = $1"}
	args := []any{id.OrgID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}

	if raw := q.Get("status"); raw != "" {
		statuses := strings.Split(raw, ",")
		for _, st := range statuses {
			if !validStatuses[st] {
				return "", nil, fmt.Errorf("estado desconocido: %s", st)
			}
		}
		add("s.status::text = ANY($%d)", statuses)
	} else {
		// Drafts belong to applicants still filling the form; they are hidden unless asked for.
		where = append(where, "s.status <> 'draft'")
	}
	if v := q.Get("form_id"); v != "" {
		add("s.form_id::text = $%d", v)
	}
	switch v := q.Get("assigned"); v {
	case "":
	case "me":
		add("s.assigned_to::text = $%d", id.UserID)
	case "none":
		where = append(where, "s.assigned_to IS NULL")
	default:
		add("s.assigned_to::text = $%d", v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		args = append(args, "%"+strings.ToLower(v)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(
			"(lower(s.applicant_email) LIKE $%d OR lower(s.applicant_name) LIKE $%d OR s.id::text LIKE $%d)", n, n, n))
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			return "", nil, errors.New("fecha 'desde' inválida")
		}
		add("COALESCE(s.submitted_at, s.created_at) >= $%d", t)
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			return "", nil, errors.New("fecha 'hasta' inválida")
		}
		add("COALESCE(s.submitted_at, s.created_at) < $%d", t.AddDate(0, 0, 1))
	}
	return " WHERE " + strings.Join(where, " AND "), args, nil
}

func submissionOrder(r *http.Request) string {
	if r.URL.Query().Get("sort") == "oldest" {
		return " ORDER BY COALESCE(s.submitted_at, s.created_at) ASC, s.id"
	}
	return " ORDER BY COALESCE(s.submitted_at, s.created_at) DESC, s.id"
}

func (s *Server) listSubmissions(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	where, args, err := submissionFilter(r, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	page := max(atoiDefault(r.URL.Query().Get("page"), 1), 1)
	pageSize := min(max(atoiDefault(r.URL.Query().Get("page_size"), 25), 1), 100)

	ctx := r.Context()
	var total int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM submissions s"+where, args...).Scan(&total); err != nil {
		s.serverError(w, r, err)
		return
	}

	rows, err := s.DB.Query(ctx,
		submissionSelect+where+submissionOrder(r)+fmt.Sprintf(" LIMIT %d OFFSET %d", pageSize, (page-1)*pageSize),
		args...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	items := []submissionRow{}
	for rows.Next() {
		sr, err := scanSubmission(rows)
		if err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		items = append(items, sr)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}

	counts, err := s.statusCounts(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "page": page, "page_size": pageSize, "counts": counts,
	})
}

// statusCounts feeds the inbox tabs; "mine" counts open submissions assigned to the viewer.
func (s *Server) statusCounts(ctx context.Context, id auth.Identity) (map[string]int, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT status::text, count(*) FROM submissions WHERE organization_id = $1 GROUP BY status
		UNION ALL
		SELECT 'mine', count(*) FROM submissions
		WHERE organization_id = $1 AND assigned_to = $2 AND status IN ('submitted', 'in_review')`,
		id.OrgID, id.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		counts[k] = n
	}
	return counts, rows.Err()
}

func (s *Server) submissionFacets(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	ctx := r.Context()

	type formRef struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	forms := []formRef{}
	rows, err := s.DB.Query(ctx,
		`SELECT id, title FROM forms WHERE organization_id = $1 ORDER BY title`, id.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for rows.Next() {
		var f formRef
		if err := rows.Scan(&f.ID, &f.Title); err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		forms = append(forms, f)
	}
	rows.Close()

	type reviewer struct {
		userRef
		Role string `json:"role"`
	}
	reviewers := []reviewer{}
	rows, err = s.DB.Query(ctx, `
		SELECT u.id, u.name, u.email, m.role FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1 ORDER BY u.name, u.email`, id.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for rows.Next() {
		var rv reviewer
		if err := rows.Scan(&rv.ID, &rv.Name, &rv.Email, &rv.Role); err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		reviewers = append(reviewers, rv)
	}
	rows.Close()
	writeJSON(w, http.StatusOK, map[string]any{"forms": forms, "reviewers": reviewers})
}

type fileRow struct {
	ID         string    `json:"id"`
	FieldKey   string    `json:"field_key"`
	Filename   string    `json:"filename"`
	MimeType   string    `json:"mime_type"`
	SizeBytes  int64     `json:"size_bytes"`
	UploadedAt time.Time `json:"uploaded_at"`
}

type commentRow struct {
	ID         string     `json:"id"`
	FieldKey   *string    `json:"field_key"`
	Body       string     `json:"body"`
	Author     userRef    `json:"author"`
	ResolvedAt *time.Time `json:"resolved_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type eventRow struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorName  string          `json:"actor_name"`
	Action     string          `json:"action"`
	FromStatus *string         `json:"from_status"`
	ToStatus   *string         `json:"to_status"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (s *Server) getSubmission(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	subID := chi.URLParam(r, "id")
	ctx := r.Context()

	sr, err := scanSubmission(s.DB.QueryRow(ctx,
		submissionSelect+" WHERE s.id::text = $1 AND s.organization_id = $2", subID, id.OrgID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Envío no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var data, schema json.RawMessage
	if err := s.DB.QueryRow(ctx, `
		SELECT s.data, v.schema FROM submissions s JOIN form_versions v ON v.id = s.form_version_id
		WHERE s.id = $1`, sr.ID).Scan(&data, &schema); err != nil {
		s.serverError(w, r, err)
		return
	}

	files := []fileRow{}
	rows, err := s.DB.Query(ctx, `
		SELECT id, field_key, filename, mime_type, size_bytes, uploaded_at
		FROM submission_files WHERE submission_id = $1 ORDER BY uploaded_at`, sr.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.ID, &f.FieldKey, &f.Filename, &f.MimeType, &f.SizeBytes, &f.UploadedAt); err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		files = append(files, f)
	}
	rows.Close()

	comments, err := s.loadComments(ctx, sr.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	events := []eventRow{}
	rows, err = s.DB.Query(ctx, `
		SELECT a.id, a.actor_type, COALESCE(NULLIF(u.name, ''), u.email, ''), a.action,
		       a.from_status, a.to_status, a.metadata, a.created_at
		FROM audit_events a
		LEFT JOIN users u ON a.actor_type = 'user' AND u.id::text = a.actor_id
		WHERE a.submission_id = $1 ORDER BY a.created_at, a.id`, sr.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorName, &e.Action, &e.FromStatus, &e.ToStatus, &e.Metadata, &e.CreatedAt); err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		events = append(events, e)
	}
	rows.Close()

	writeJSON(w, http.StatusOK, map[string]any{
		"submission":          sr,
		"data":                data,
		"schema":              schema,
		"files":               files,
		"comments":            comments,
		"events":              events,
		"allowed_transitions": allowedTransitions(sr.Status, id.Role),
	})
}

func (s *Server) loadComments(ctx context.Context, submissionID string) ([]commentRow, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT c.id, c.field_key, c.body, u.id, u.name, u.email, c.resolved_at, c.created_at
		FROM review_comments c JOIN users u ON u.id = c.author_user_id
		WHERE c.submission_id = $1 ORDER BY c.created_at, c.field_key NULLS FIRST, c.id`, submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	comments := []commentRow{}
	for rows.Next() {
		var c commentRow
		if err := rows.Scan(&c.ID, &c.FieldKey, &c.Body, &c.Author.ID, &c.Author.Name, &c.Author.Email, &c.ResolvedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// transitions lists which statuses a reviewer can move a submission to.
// changes_requested -> submitted happens when the applicant resubmits (portal side).
var transitions = map[string][]string{
	"submitted":         {"in_review", "changes_requested", "approved", "rejected"},
	"in_review":         {"changes_requested", "approved", "rejected"},
	"changes_requested": {"in_review", "rejected"},
	"approved":          {"in_review"},
	"rejected":          {"in_review"},
}

func allowedTransitions(from, role string) []string {
	out := []string{}
	for _, to := range transitions[from] {
		if (from == "approved" || from == "rejected") && role == "reviewer" {
			continue // reopening a decision is for owners and admins
		}
		out = append(out, to)
	}
	return out
}

func transitionAction(from, to string) string {
	switch {
	case to == "in_review" && (from == "approved" || from == "rejected"):
		return "reopened"
	case to == "in_review" && from == "changes_requested":
		return "changes_request_withdrawn"
	case to == "in_review":
		return "review_started"
	default:
		return to // changes_requested, approved, rejected
	}
}

type fieldComment struct {
	FieldKey string `json:"field_key"`
	Body     string `json:"body"`
}

func (s *Server) transitionSubmission(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		To            string         `json:"to"`
		From          string         `json:"from"` // optional: reject if the status changed meanwhile
		Comment       string         `json:"comment"`
		FieldComments []fieldComment `json:"field_comments"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	in.Comment = strings.TrimSpace(in.Comment)
	fieldComments := in.FieldComments[:0]
	for _, fc := range in.FieldComments {
		fc.FieldKey, fc.Body = strings.TrimSpace(fc.FieldKey), strings.TrimSpace(fc.Body)
		if fc.FieldKey != "" && fc.Body != "" {
			fieldComments = append(fieldComments, fc)
		}
	}
	switch in.To {
	case "changes_requested":
		if in.Comment == "" && len(fieldComments) == 0 {
			writeError(w, http.StatusBadRequest, "Indica qué debe corregir el solicitante", nil)
			return
		}
	case "rejected":
		if in.Comment == "" {
			writeError(w, http.StatusBadRequest, "Indica el motivo del rechazo", nil)
			return
		}
	}

	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	var subID, from string
	var assigned *string
	err = tx.QueryRow(ctx, `
		SELECT id, status::text, assigned_to::text FROM submissions
		WHERE id::text = $1 AND organization_id = $2 FOR UPDATE`, chi.URLParam(r, "id"), id.OrgID,
	).Scan(&subID, &from, &assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Envío no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if in.From != "" && in.From != from {
		writeError(w, http.StatusConflict, "El envío cambió de estado mientras lo revisabas; recarga la página", nil)
		return
	}
	allowed := false
	for _, to := range allowedTransitions(from, id.Role) {
		allowed = allowed || to == in.To
	}
	if !allowed {
		writeError(w, http.StatusConflict, fmt.Sprintf("No se puede pasar de %s a %s", from, in.To), nil)
		return
	}

	// Taking a submission into review assigns it to the reviewer if nobody has it.
	assignSelf := in.To == "in_review" && assigned == nil
	if _, err := tx.Exec(ctx, `
		UPDATE submissions SET
			status = $2::submission_status,
			decided_at = CASE WHEN $2 IN ('approved', 'rejected') THEN now() ELSE NULL END,
			assigned_to = CASE WHEN $3 THEN $4::uuid ELSE assigned_to END,
			updated_at = now()
		WHERE id = $1`, subID, in.To, assignSelf, id.UserID); err != nil {
		s.serverError(w, r, err)
		return
	}

	if in.Comment != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO review_comments (submission_id, author_user_id, body) VALUES ($1, $2, $3)`,
			subID, id.UserID, in.Comment); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	for _, fc := range fieldComments {
		if _, err := tx.Exec(ctx,
			`INSERT INTO review_comments (submission_id, author_user_id, field_key, body) VALUES ($1, $2, $3, $4)`,
			subID, id.UserID, fc.FieldKey, fc.Body); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	meta := map[string]any{}
	if in.Comment != "" {
		meta["comment"] = in.Comment
	}
	if len(fieldComments) > 0 {
		meta["field_comments"] = fieldComments
	}
	if assignSelf {
		meta["assigned_to"] = id.UserID
	}
	if err := insertAudit(ctx, tx, subID, id.UserID, transitionAction(from, in.To), &from, &in.To, meta); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": in.To})
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in fieldComment
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	in.FieldKey = strings.TrimSpace(in.FieldKey)
	if in.Body == "" {
		writeError(w, http.StatusBadRequest, "El comentario está vacío", nil)
		return
	}
	var fieldKey *string
	if in.FieldKey != "" {
		fieldKey = &in.FieldKey
	}

	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	var commentID, subID string
	err = tx.QueryRow(ctx, `
		INSERT INTO review_comments (submission_id, author_user_id, field_key, body)
		SELECT s.id, $3, $4, $5 FROM submissions s WHERE s.id::text = $1 AND s.organization_id = $2
		RETURNING id, submission_id`, chi.URLParam(r, "id"), id.OrgID, id.UserID, fieldKey, in.Body,
	).Scan(&commentID, &subID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Envío no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	meta := map[string]any{"comment_id": commentID, "body": in.Body}
	if fieldKey != nil {
		meta["field_key"] = *fieldKey
	}
	if err := insertAudit(ctx, tx, subID, id.UserID, "commented", nil, nil, meta); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": commentID})
}

func (s *Server) resolveComment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	var subID string
	err = tx.QueryRow(ctx, `
		UPDATE review_comments c SET resolved_at = now()
		FROM submissions s
		WHERE c.submission_id = s.id AND s.id::text = $1 AND c.id::text = $2
		  AND s.organization_id = $3 AND c.resolved_at IS NULL
		RETURNING s.id`, chi.URLParam(r, "id"), chi.URLParam(r, "commentID"), id.OrgID,
	).Scan(&subID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Comentario no encontrado o ya resuelto", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := insertAudit(ctx, tx, subID, id.UserID, "comment_resolved", nil, nil,
		map[string]any{"comment_id": chi.URLParam(r, "commentID")}); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) assignSubmission(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		UserID *string `json:"user_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	if in.UserID != nil && *in.UserID == "" {
		in.UserID = nil
	}

	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	if in.UserID != nil {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id::text = $1 AND organization_id = $2)`,
			*in.UserID, id.OrgID).Scan(&ok); err != nil {
			s.serverError(w, r, err)
			return
		}
		if !ok {
			writeError(w, http.StatusBadRequest, "Ese usuario no pertenece a la organización", nil)
			return
		}
	}

	var subID string
	var previous *string
	err = tx.QueryRow(ctx, `
		WITH old AS (
			SELECT id, assigned_to FROM submissions
			WHERE id::text = $1 AND organization_id = $2 FOR UPDATE
		)
		UPDATE submissions s SET assigned_to = $3::uuid, updated_at = now()
		FROM old WHERE s.id = old.id
		RETURNING s.id, old.assigned_to::text`, chi.URLParam(r, "id"), id.OrgID, in.UserID,
	).Scan(&subID, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Envío no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := insertAudit(ctx, tx, subID, id.UserID, "assigned", nil, nil,
		map[string]any{"assigned_to": in.UserID, "previous": previous}); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func insertAudit(ctx context.Context, tx pgx.Tx, submissionID, userID, action string, from, to *string, meta map[string]any) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (submission_id, actor_type, actor_id, action, from_status, to_status, metadata)
		VALUES ($1, 'user', $2, $3, $4::submission_status, $5::submission_status, $6)`,
		submissionID, userID, action, from, to, b)
	return err
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
