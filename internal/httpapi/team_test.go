package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var linkRe = regexp.MustCompile(`https://forms\.example\.com/admin/contrasena\?token=([A-Za-z0-9_-]+)`)

func tokenFrom(t *testing.T, text string) string {
	t.Helper()
	m := linkRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no password link in %q", text)
	}
	return m[1]
}

func TestTeamManagement(t *testing.T) {
	srv, pool, mail := adminTestServer(t)
	ownerEmail, _, _ := seed(t, pool)
	owner := newClient(t, srv.URL)
	if code, body := owner.do("POST", "/api/v1/auth/login", map[string]string{"email": ownerEmail, "password": "secreto-123"}); code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}

	// Invite a reviewer.
	revEmail := "rev-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	if code, _ := owner.do("POST", "/api/v1/admin/team", map[string]string{"email": revEmail, "role": "boss"}); code != http.StatusBadRequest {
		t.Fatalf("bad role: %d", code)
	}
	if code, _ := owner.do("POST", "/api/v1/admin/team", map[string]string{"email": "no es email", "role": "reviewer"}); code != http.StatusBadRequest {
		t.Fatalf("bad email: %d", code)
	}
	code, body := owner.do("POST", "/api/v1/admin/team", map[string]string{"email": strings.ToUpper(revEmail), "name": "Rita Revisora", "role": "reviewer"})
	if code != http.StatusCreated {
		t.Fatalf("invite: %d %s", code, body)
	}
	var invited struct {
		Member    memberRow `json:"member"`
		InviteURL string    `json:"invite_url"`
	}
	_ = json.Unmarshal(body, &invited)
	if invited.Member.Email != revEmail || !invited.Member.Pending || invited.Member.Role != "reviewer" {
		t.Fatalf("invited member: %s", body)
	}
	m := mail.next(t)
	if m.To != revEmail || !strings.Contains(m.Text, "Revisor") {
		t.Fatalf("invite email: %+v", m)
	}
	token := tokenFrom(t, m.Text)
	if !strings.HasSuffix(invited.InviteURL, token) {
		t.Fatalf("invite_url %q does not match emailed token", invited.InviteURL)
	}
	if code, _ := owner.do("POST", "/api/v1/admin/team", map[string]string{"email": revEmail, "role": "reviewer"}); code != http.StatusConflict {
		t.Fatalf("duplicate invite: %d", code)
	}

	// The invitee cannot log in until they set a password through the link.
	rev := newClient(t, srv.URL)
	code, body = rev.do("GET", "/api/v1/auth/password/token?token="+url.QueryEscape(token), nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"purpose":"invite"`) {
		t.Fatalf("check token: %d %s", code, body)
	}
	if code, _ := rev.do("POST", "/api/v1/auth/password/reset", map[string]string{"token": token, "password": "corta"}); code != http.StatusBadRequest {
		t.Fatalf("weak password: %d", code)
	}
	if code, body := rev.do("POST", "/api/v1/auth/password/reset", map[string]string{"token": token, "password": "clave-segura-1"}); code != http.StatusOK {
		t.Fatalf("set password: %d %s", code, body)
	}
	if code, _ := rev.do("POST", "/api/v1/auth/password/reset", map[string]string{"token": token, "password": "otra-clave-2"}); code != http.StatusNotFound {
		t.Fatalf("token reuse: %d", code)
	}

	// Reviewers review, but do not build forms, see the activity log or manage the team.
	if code, _ := rev.do("GET", "/api/v1/admin/submissions", nil); code != http.StatusOK {
		t.Fatalf("reviewer inbox: %d", code)
	}
	for _, path := range []string{"/api/v1/admin/forms", "/api/v1/admin/activity"} {
		if code, _ := rev.do("GET", path, nil); code != http.StatusForbidden {
			t.Fatalf("reviewer %s: %d", path, code)
		}
	}
	if code, _ := rev.do("POST", "/api/v1/admin/team", map[string]string{"email": "x@example.com", "role": "reviewer"}); code != http.StatusForbidden {
		t.Fatalf("reviewer invite: %d", code)
	}
	if code, _ := rev.do("GET", "/api/v1/admin/team", nil); code != http.StatusOK {
		t.Fatalf("reviewer team list: %d", code)
	}

	// Change own password.
	if code, _ := rev.do("PUT", "/api/v1/auth/me/password", map[string]string{"current_password": "mala", "password": "nueva-clave-3"}); code != http.StatusBadRequest {
		t.Fatalf("wrong current password: %d", code)
	}
	if code, _ := rev.do("PUT", "/api/v1/auth/me/password", map[string]string{"current_password": "clave-segura-1", "password": "nueva-clave-3"}); code != http.StatusNoContent {
		t.Fatalf("change password: %d", code)
	}
	if code, _ := rev.do("GET", "/api/v1/auth/me", nil); code != http.StatusOK {
		t.Fatalf("current session must survive a password change: %d", code)
	}

	// Role rules.
	var me struct {
		ID string `json:"id"`
	}
	_, body = owner.do("GET", "/api/v1/auth/me", nil)
	_ = json.Unmarshal(body, &me)
	if code, _ := owner.do("PATCH", "/api/v1/admin/team/"+me.ID, map[string]string{"role": "admin"}); code != http.StatusConflict {
		t.Fatalf("self demotion: %d", code)
	}
	revID := invited.Member.ID
	if code, body := owner.do("PATCH", "/api/v1/admin/team/"+revID, map[string]string{"role": "admin"}); code != http.StatusOK {
		t.Fatalf("promote: %d %s", code, body)
	}
	if code, _ := rev.do("PATCH", "/api/v1/admin/team/"+me.ID, map[string]any{"active": false}); code != http.StatusForbidden {
		t.Fatalf("admin deactivating owner: %d", code)
	}
	if code, _ := rev.do("POST", "/api/v1/admin/team", map[string]string{"email": "o2@example.com", "role": "owner"}); code != http.StatusForbidden {
		t.Fatalf("admin inviting owner: %d", code)
	}
	if code, _ := rev.do("GET", "/api/v1/admin/activity", nil); code != http.StatusOK {
		t.Fatalf("admin activity: %d", code)
	}

	// Deactivating signs the member out and blocks login and password links.
	if code, body := owner.do("PATCH", "/api/v1/admin/team/"+revID, map[string]any{"active": false}); code != http.StatusOK || !strings.Contains(string(body), `"active":false`) {
		t.Fatalf("deactivate: %d %s", code, body)
	}
	if code, _ := rev.do("GET", "/api/v1/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("deactivated session: %d", code)
	}
	if code, _ := rev.do("POST", "/api/v1/auth/login", map[string]string{"email": revEmail, "password": "nueva-clave-3"}); code != http.StatusUnauthorized {
		t.Fatalf("deactivated login: %d", code)
	}
	if code, _ := rev.do("POST", "/api/v1/auth/password/forgot", map[string]string{"email": revEmail}); code != http.StatusNoContent {
		t.Fatalf("forgot (disabled): %d", code)
	}
	_, body = owner.do("GET", "/api/v1/admin/submissions/facets", nil)
	if strings.Contains(string(body), revEmail) {
		t.Fatalf("deactivated member offered as assignee: %s", body)
	}
	if code, _ := owner.do("PATCH", "/api/v1/admin/team/"+revID, map[string]any{"active": true}); code != http.StatusOK {
		t.Fatalf("reactivate: %d", code)
	}

	// Forgot password: unknown emails get the same answer and no email.
	if code, _ := owner.do("POST", "/api/v1/auth/password/forgot", map[string]string{"email": "nadie@example.com"}); code != http.StatusNoContent {
		t.Fatalf("forgot unknown: %d", code)
	}
	if code, _ := owner.do("POST", "/api/v1/auth/password/forgot", map[string]string{"email": revEmail}); code != http.StatusNoContent {
		t.Fatalf("forgot: %d", code)
	}
	m = mail.next(t)
	if m.To != revEmail || !strings.Contains(m.Subject, "contraseña") {
		t.Fatalf("reset email: %+v", m)
	}
	if code, _ := rev.do("POST", "/api/v1/auth/password/reset", map[string]string{"token": tokenFrom(t, m.Text), "password": "recuperada-4"}); code != http.StatusOK {
		t.Fatalf("reset: %d", code)
	}

	// The activity log records team changes.
	code, body = owner.do("GET", "/api/v1/admin/activity?scope=team", nil)
	if code != http.StatusOK {
		t.Fatalf("activity: %d %s", code, body)
	}
	var act struct {
		Items []activityRow `json:"items"`
	}
	_ = json.Unmarshal(body, &act)
	var actions []string
	for _, e := range act.Items {
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, ","); got != "member.reactivated,member.deactivated,member.role_changed,member.invited" {
		t.Fatalf("team activity: %s", got)
	}
	if act.Items[0].ActorName != "Ana Admin" || act.Items[0].Submission != nil {
		t.Fatalf("activity row: %+v", act.Items[0])
	}
	code, body = owner.do("GET", "/api/v1/admin/activity?limit=1", nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"next_before":`) || strings.Contains(string(body), `"next_before":null`) {
		t.Fatalf("activity paging: %d %s", code, body)
	}
}
