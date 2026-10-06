package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// addMember creates an active member in the submission's organization and returns a logged-in client.
func addMember(t *testing.T, pool *pgxpool.Pool, base, subID, name, role string) (*client, string) {
	t.Helper()
	ctx := context.Background()
	email := strings.ToLower(name) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	hash, err := auth.HashPassword("secreto-123")
	if err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`, email, name, hash).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memberships (user_id, organization_id, role)
		SELECT $1, organization_id, $3 FROM submissions WHERE id = $2`, userID, subID, role); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, base)
	if code, body := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "secreto-123"}); code != http.StatusOK {
		t.Fatalf("login %s: %d %s", name, code, body)
	}
	return c, userID
}

type detailWithApproval struct {
	Submission submissionRow  `json:"submission"`
	Approval   *approvalState `json:"approval"`
}

func getDetail(t *testing.T, c *client, subID string) detailWithApproval {
	t.Helper()
	code, body := c.do("GET", "/api/v1/admin/submissions/"+subID, nil)
	if code != http.StatusOK {
		t.Fatalf("detail: %d %s", code, body)
	}
	var d detailWithApproval
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMultiLevelApproval(t *testing.T) {
	srv, pool, mail := adminTestServer(t)
	ownerEmail, subID, _ := seed(t, pool)
	owner := newClient(t, srv.URL)
	if code, _ := owner.do("POST", "/api/v1/auth/login", map[string]string{"email": ownerEmail, "password": "secreto-123"}); code != http.StatusOK {
		t.Fatal("owner login")
	}
	rita, _ := addMember(t, pool, srv.URL, subID, "Rita", "reviewer")
	carlos, carlosID := addMember(t, pool, srv.URL, subID, "Carlos", "admin")
	var formID string
	if err := pool.QueryRow(context.Background(), `SELECT form_id FROM submissions WHERE id = $1`, subID).Scan(&formID); err != nil {
		t.Fatal(err)
	}
	flowURL := "/api/v1/admin/forms/" + formID + "/approval-flow"
	transition := func(c *client, to string) (int, string) {
		code, body := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": to})
		return code, string(body)
	}

	// Configuration is validated and only for owners and admins.
	if code, _ := rita.do("PUT", flowURL, map[string]any{"steps": []approvalStep{{Name: "X"}}}); code != http.StatusForbidden {
		t.Fatalf("reviewer configures flow: %d", code)
	}
	for _, bad := range [][]approvalStep{
		{{Name: "A"}, {Name: "a"}},
		{{Name: " "}},
		{{Name: "A", Approvers: []string{"00000000-0000-0000-0000-000000000000"}}},
		{{Name: "1"}, {Name: "2"}, {Name: "3"}, {Name: "4"}, {Name: "5"}, {Name: "6"}},
	} {
		if code, body := owner.do("PUT", flowURL, map[string]any{"steps": bad}); code != http.StatusBadRequest {
			t.Fatalf("invalid flow %v accepted: %d %s", bad, code, body)
		}
	}
	code, body := owner.do("PUT", flowURL, map[string]any{"steps": []approvalStep{
		{Name: "Comercial"},
		{Name: "Cumplimiento", Approvers: []string{carlosID, carlosID}},
	}})
	if code != http.StatusOK || !strings.Contains(string(body), `"name":"Carlos"`) {
		t.Fatalf("save flow: %d %s", code, body)
	}

	// Step 1: any member signs off; the submission stays in review and goes to Carlos.
	if code, body := transition(rita, "approved"); code != http.StatusOK || !strings.Contains(body, `"status":"in_review"`) {
		t.Fatalf("step 1: %d %s", code, body)
	}
	if m := mail.next(t); !strings.HasPrefix(m.Subject, "Estamos revisando") {
		t.Fatalf("expected only the in-review email, got %+v", m)
	}
	d := getDetail(t, rita, subID)
	if d.Submission.Status != "in_review" || d.Submission.AssignedTo == nil || d.Submission.AssignedTo.ID != carlosID {
		t.Fatalf("after step 1: %+v", d.Submission)
	}
	if d.Approval == nil || d.Approval.Current != 1 || d.Approval.CanApprove || len(d.Approval.Approvals) != 1 {
		t.Fatalf("approval state for rita: %+v", d.Approval)
	}
	if d.Submission.ApprovalStep == nil || d.Submission.ApprovalStep.Name != "Cumplimiento" || d.Submission.ApprovalStep.Total != 2 {
		t.Fatalf("approval progress: %+v", d.Submission.ApprovalStep)
	}
	if code, _ := transition(rita, "approved"); code != http.StatusForbidden {
		t.Fatalf("rita signs off a step reserved to Carlos: %d", code)
	}

	// Step 2: Carlos approves for real.
	if !getDetail(t, carlos, subID).Approval.CanApprove {
		t.Fatal("carlos should be able to approve")
	}
	if code, body := transition(carlos, "approved"); code != http.StatusOK || !strings.Contains(body, `"status":"approved"`) {
		t.Fatalf("step 2: %d %s", code, body)
	}
	if m := mail.next(t); !strings.HasPrefix(m.Subject, "Solicitud aprobada") {
		t.Fatalf("approval email: %+v", m)
	}

	// Reopening restarts the flow and voids the sign-offs.
	if code, body := transition(owner, "in_review"); code != http.StatusOK {
		t.Fatalf("reopen: %d %s", code, body)
	}
	d = getDetail(t, owner, subID)
	if d.Approval.Current != 0 || len(d.Approval.Approvals) != 2 || d.Approval.Approvals[0].Invalidated == nil {
		t.Fatalf("after reopen: %+v", d.Approval)
	}

	// Four eyes: Carlos signs step 1 and cannot sign step 2 too; an owner can.
	if code, body := transition(carlos, "approved"); code != http.StatusOK {
		t.Fatalf("carlos step 1: %d %s", code, body)
	}
	code, body2 := transition(carlos, "approved")
	if code != http.StatusForbidden || !strings.Contains(body2, "Ya aprobaste") {
		t.Fatalf("four eyes: %d %s", code, body2)
	}
	if code, body := transition(owner, "approved"); code != http.StatusOK || !strings.Contains(body, `"status":"approved"`) {
		t.Fatalf("owner signs off: %d %s", code, body)
	}

	// The audit trail names the steps, and the config change shows in the activity log.
	var actions string
	if err := pool.QueryRow(context.Background(), `
		SELECT string_agg(action, ',' ORDER BY id) FROM audit_events WHERE submission_id = $1`, subID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != "step_approved,approved,reopened,step_approved,approved" {
		t.Fatalf("audit: %s", actions)
	}
	_, act := owner.do("GET", "/api/v1/admin/activity?scope=team", nil)
	if !strings.Contains(string(act), "form.approval_flow_updated") {
		t.Fatalf("activity: %s", act)
	}

	// Removing the flow brings back the plain one-click approval.
	if code, _ := owner.do("PUT", flowURL, map[string]any{"steps": []approvalStep{}}); code != http.StatusOK {
		t.Fatal("clear flow")
	}
	if d := getDetail(t, owner, subID); d.Submission.ApprovalStep != nil {
		t.Fatalf("no flow, no progress: %+v", d.Submission.ApprovalStep)
	}
}
