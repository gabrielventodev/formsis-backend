package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gabrielventodev/formsis/api/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Multi-level approval flows. A form may define ordered steps, each with an
// optional list of approvers (empty = any member). "Approve" on a submission
// signs off the pending step; only the last step's sign-off approves it.
// Rules:
//   - a step with approvers can be signed off by them or by an owner;
//   - with two or more steps, one person signs off at most one step
//     (four-eyes principle), owners included;
//   - requesting changes or reopening a decision restarts the flow.

const maxApprovalSteps = 5

type approvalStep struct {
	Name      string   `json:"name"`
	Approvers []string `json:"approvers"`
}

type approvalStepView struct {
	Name      string    `json:"name"`
	Approvers []userRef `json:"approvers"`
}

// ---- Configuration (owners and admins, under /admin/forms/{formID}) ----

func (s *Server) getApprovalFlow(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	steps, err := loadFormSteps(r.Context(), s.DB.QueryRow(r.Context(),
		`SELECT approval_steps FROM forms WHERE id::text = $1 AND organization_id = $2`, chi.URLParam(r, "formID"), id.OrgID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Formulario no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	views, err := s.stepViews(r.Context(), id.OrgID, steps)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": views})
}

func (s *Server) putApprovalFlow(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		Steps []approvalStep `json:"steps"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	if len(in.Steps) > maxApprovalSteps {
		writeError(w, http.StatusBadRequest, "Usa como máximo 5 pasos de aprobación", nil)
		return
	}
	ctx := r.Context()
	steps := make([]approvalStep, 0, len(in.Steps))
	names := map[string]bool{}
	for i, st := range in.Steps {
		st.Name = strings.TrimSpace(st.Name)
		if st.Name == "" || utf8.RuneCountInString(st.Name) > 80 {
			writeError(w, http.StatusBadRequest, "Cada paso necesita un nombre de hasta 80 caracteres", nil)
			return
		}
		if names[strings.ToLower(st.Name)] {
			writeError(w, http.StatusBadRequest, "Hay dos pasos con el mismo nombre: "+st.Name, nil)
			return
		}
		names[strings.ToLower(st.Name)] = true
		approvers := []string{}
		for _, a := range st.Approvers {
			if !slices.Contains(approvers, a) {
				approvers = append(approvers, a)
			}
		}
		if len(approvers) > 0 {
			var n int
			if err := s.DB.QueryRow(ctx, `
				SELECT count(*) FROM memberships
				WHERE organization_id = $1 AND disabled_at IS NULL AND user_id::text = ANY($2)`,
				id.OrgID, approvers).Scan(&n); err != nil {
				s.serverError(w, r, err)
				return
			}
			if n != len(approvers) {
				writeError(w, http.StatusBadRequest, "El paso "+strconv.Itoa(i+1)+" tiene aprobadores que no son miembros activos", nil)
				return
			}
		}
		steps = append(steps, approvalStep{Name: st.Name, Approvers: approvers})
	}
	raw, _ := json.Marshal(steps)

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	var formID, title string
	err = tx.QueryRow(ctx, `
		UPDATE forms SET approval_steps = $3, updated_at = now()
		WHERE id::text = $1 AND organization_id = $2 RETURNING id, title`,
		chi.URLParam(r, "formID"), id.OrgID, raw).Scan(&formID, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Formulario no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stepNames := make([]string, len(steps))
	for i, st := range steps {
		stepNames[i] = st.Name
	}
	if err := insertOrgAudit(ctx, tx, id.OrgID, id.UserID, "form.approval_flow_updated",
		map[string]any{"form_id": formID, "form_title": title, "steps": stepNames}); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	views, err := s.stepViews(ctx, id.OrgID, steps)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": views})
}

func loadFormSteps(_ context.Context, row pgx.Row) ([]approvalStep, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return nil, err
	}
	steps := []approvalStep{}
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, err
	}
	return steps, nil
}

// stepViews resolves approver ids to names; members no longer active are dropped.
func (s *Server) stepViews(ctx context.Context, orgID string, steps []approvalStep) ([]approvalStepView, error) {
	ids := []string{}
	for _, st := range steps {
		ids = append(ids, st.Approvers...)
	}
	users := map[string]userRef{}
	if len(ids) > 0 {
		rows, err := s.DB.Query(ctx, `
			SELECT u.id, u.name, u.email FROM users u JOIN memberships m ON m.user_id = u.id
			WHERE m.organization_id = $1 AND m.disabled_at IS NULL AND u.id::text = ANY($2)`, orgID, ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var u userRef
			if err := rows.Scan(&u.ID, &u.Name, &u.Email); err != nil {
				rows.Close()
				return nil, err
			}
			users[u.ID] = u
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]approvalStepView, len(steps))
	for i, st := range steps {
		out[i] = approvalStepView{Name: st.Name, Approvers: []userRef{}}
		for _, a := range st.Approvers {
			if u, ok := users[a]; ok {
				out[i].Approvers = append(out[i].Approvers, u)
			}
		}
	}
	return out, nil
}

// ---- Sign-offs ----

type approvalRow struct {
	Step        int        `json:"step"` // 0-based
	StepName    string     `json:"step_name"`
	User        userRef    `json:"user"`
	Comment     string     `json:"comment"`
	CreatedAt   time.Time  `json:"created_at"`
	Invalidated *time.Time `json:"invalidated_at"`
}

// approvalState is what the review page needs to show the flow's progress.
type approvalState struct {
	Steps      []approvalStepView `json:"steps"`
	Current    int                `json:"current"` // index of the pending step
	Approvals  []approvalRow      `json:"approvals"`
	CanApprove bool               `json:"can_approve"`
	Reason     string             `json:"reason,omitempty"` // why the viewer cannot sign off
}

// stepDecision is the outcome of checking a sign-off on the pending step.
type stepDecision struct {
	index int
	name  string
	final bool
	next  *approvalStep
}

var errStepForbidden = errors.New("step forbidden")

// checkStep decides whether the user may sign off the pending step. It
// returns nil when the form has no approval flow (a plain approval).
func checkStep(ctx context.Context, q pgx.Tx, subID string, current int, id auth.Identity) (*stepDecision, string, error) {
	steps, err := loadFormSteps(ctx, q.QueryRow(ctx,
		`SELECT f.approval_steps FROM submissions s JOIN forms f ON f.id = s.form_id WHERE s.id = $1`, subID))
	if err != nil || len(steps) == 0 {
		return nil, "", err
	}
	if current >= len(steps) { // the flow was shortened while this submission was in review
		current = len(steps) - 1
	}
	st := steps[current]
	if len(st.Approvers) > 0 && !slices.Contains(st.Approvers, id.UserID) && id.Role != "owner" {
		return nil, "Este paso (" + st.Name + ") lo aprueba otra persona", errStepForbidden
	}
	if len(steps) > 1 {
		var signed bool
		if err := q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM submission_approvals
			               WHERE submission_id = $1 AND user_id = $2 AND invalidated_at IS NULL)`,
			subID, id.UserID).Scan(&signed); err != nil {
			return nil, "", err
		}
		if signed {
			return nil, "Ya aprobaste un paso anterior; otra persona debe aprobar " + st.Name, errStepForbidden
		}
	}
	d := &stepDecision{index: current, name: st.Name, final: current == len(steps)-1}
	if !d.final {
		d.next = &steps[current+1]
	}
	return d, "", nil
}

// loadApprovalState builds the flow summary for the review page, or nil when
// the form has no flow and the submission was never signed off.
func (s *Server) loadApprovalState(ctx context.Context, sr submissionRow, id auth.Identity) (*approvalState, error) {
	tx, err := s.DB.Begin(ctx) // read-only; a tx lets checkStep share code with the transition
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var current int
	steps, err := loadFormSteps(ctx, tx.QueryRow(ctx,
		`SELECT f.approval_steps FROM submissions s JOIN forms f ON f.id = s.form_id WHERE s.id = $1`, sr.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT approval_step FROM submissions WHERE id = $1`, sr.ID).Scan(&current); err != nil {
		return nil, err
	}

	approvals := []approvalRow{}
	rows, err := tx.Query(ctx, `
		SELECT a.step, a.step_name, u.id, u.name, u.email, a.comment, a.created_at, a.invalidated_at
		FROM submission_approvals a JOIN users u ON u.id = a.user_id
		WHERE a.submission_id = $1 ORDER BY a.created_at, a.id`, sr.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a approvalRow
		if err := rows.Scan(&a.Step, &a.StepName, &a.User.ID, &a.User.Name, &a.User.Email, &a.Comment, &a.CreatedAt, &a.Invalidated); err != nil {
			rows.Close()
			return nil, err
		}
		approvals = append(approvals, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(steps) == 0 && len(approvals) == 0 {
		return nil, nil
	}

	views, err := s.stepViews(ctx, id.OrgID, steps)
	if err != nil {
		return nil, err
	}
	if len(steps) > 0 && current >= len(steps) {
		current = len(steps) - 1
	}
	st := &approvalState{Steps: views, Current: current, Approvals: approvals}
	if slices.Contains(allowedTransitions(sr.Status, id.Role), "approved") && len(steps) > 0 {
		_, reason, err := checkStep(ctx, tx, sr.ID, current, id)
		switch {
		case errors.Is(err, errStepForbidden):
			st.Reason = reason
		case err != nil:
			return nil, err
		default:
			st.CanApprove = true
		}
	}
	return st, nil
}
