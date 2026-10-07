package portal

import (
	"context"
	"strings"
	"testing"
)

func TestLivenessHandoff(t *testing.T) {
	ff := &fakeFace{decisions: []string{"retry", "pass"}}
	srv, pool, tok := livenessSetup(t, ff)

	// Only liveness fields can be handed off.
	if code, _ := call(t, srv, "POST", "/submission/liveness/handoff", tok, map[string]string{"fieldKey": "nombre"}); code != 400 {
		t.Fatalf("handoff for text field: %d", code)
	}
	code, ho := call(t, srv, "POST", "/submission/liveness/handoff", tok, map[string]string{"fieldKey": "vida"})
	if code != 201 {
		t.Fatalf("handoff: %d %v", code, ho)
	}
	hoID := ho["id"].(string)
	_, phone, ok := strings.Cut(ho["url"].(string), "/v/")
	if !ok || phone == "" {
		t.Fatalf("url: %v", ho["url"])
	}
	status := func() map[string]any {
		t.Helper()
		code, st := call(t, srv, "GET", "/submission/liveness/handoff/"+hoID, tok, nil)
		if code != 200 {
			t.Fatalf("status: %d %v", code, st)
		}
		return st
	}
	if st := status(); st["openedAt"] != nil || st["checking"] != false || st["expired"] != false {
		t.Fatalf("fresh status: %v", st)
	}

	// The phone token is not an access token, and the access token doesn't open the phone routes.
	if code, _ := call(t, srv, "GET", "/submission", phone, nil); code != 401 {
		t.Fatalf("phone token on /submission: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/handoff", tok, nil); code != 401 {
		t.Fatalf("access token on /handoff: %d", code)
	}

	code, info := call(t, srv, "GET", "/handoff", phone, nil)
	if code != 200 || info["field"].(map[string]any)["label"] != "Verifica que eres tú" || info["completed"] != false {
		t.Fatalf("phone info: %d %v", code, info)
	}
	if _, leaks := info["applicant"]; leaks {
		t.Fatalf("phone sees applicant data: %v", info)
	}

	// The phone takes the challenge of the handed-off field, whatever it asks for.
	code, ch := call(t, srv, "POST", "/handoff/liveness", phone, map[string]string{"fieldKey": "nombre"})
	if code != 201 {
		t.Fatalf("phone challenge: %d %v", code, ch)
	}
	if st := status(); st["openedAt"] == nil || st["checking"] != true {
		t.Fatalf("status while checking: %v", st)
	}
	code, res := postFrames(t, srv, "/handoff/liveness/"+ch["id"].(string), phone, []int{0, 1, 2}, jpegFrame)
	if code != 200 || res["decision"] != "retry" {
		t.Fatalf("phone retry: %d %v", code, res)
	}

	// A challenge started on the computer can't be answered from the phone.
	own := challenge(t, srv, tok)
	if code, _ := postFrames(t, srv, "/handoff/liveness/"+own["id"].(string), phone, []int{0, 1, 2}, jpegFrame); code != 404 {
		t.Fatalf("phone answers computer challenge: %d", code)
	}

	_, ch = call(t, srv, "POST", "/handoff/liveness", phone, nil)
	code, res = postFrames(t, srv, "/handoff/liveness/"+ch["id"].(string), phone, []int{0, 1, 2}, jpegFrame)
	if code != 200 || res["decision"] != "pass" || res["completed"] != true {
		t.Fatalf("phone pass: %d %v", code, res)
	}

	// The computer sees both phone attempts; the link stops working once the field is done.
	st := status()
	attempts := st["attempts"].([]any)
	if len(attempts) != 2 || attempts[1].(map[string]any)["decision"] != "pass" || st["checking"] != false || st["expired"] != true {
		t.Fatalf("final status: %v", st)
	}
	if code, _ := call(t, srv, "GET", "/handoff", phone, nil); code != 401 {
		t.Fatalf("used link still works: %d", code)
	}
	var fromPhone int
	must(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM liveness_checks WHERE handoff_id IS NOT NULL`).Scan(&fromPhone))
	if fromPhone < 2 {
		t.Fatalf("phone attempts not linked: %d", fromPhone)
	}
	if code, _ := call(t, srv, "POST", "/submission/submit", tok, nil); code != 200 {
		t.Fatalf("submit after phone check: %d", code)
	}
}

func TestLivenessHandoffReplaced(t *testing.T) {
	srv, _, tok := livenessSetup(t, &fakeFace{})
	_, first := call(t, srv, "POST", "/submission/liveness/handoff", tok, map[string]string{"fieldKey": "vida"})
	_, second := call(t, srv, "POST", "/submission/liveness/handoff", tok, map[string]string{"fieldKey": "vida"})
	_, oldTok, _ := strings.Cut(first["url"].(string), "/v/")
	_, newTok, _ := strings.Cut(second["url"].(string), "/v/")
	if code, _ := call(t, srv, "GET", "/handoff", oldTok, nil); code != 401 {
		t.Fatalf("replaced link still works: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/handoff", newTok, nil); code != 200 {
		t.Fatalf("new link: %d", code)
	}
	// Another applicant can't read this handoff's status.
	_, _, other := livenessSetup(t, &fakeFace{})
	if code, _ := call(t, srv, "GET", "/submission/liveness/handoff/"+second["id"].(string), other, nil); code != 401 && code != 404 {
		t.Fatalf("foreign status: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/submission/liveness/handoff/nope", tok, nil); code != 404 {
		t.Fatalf("bad id: %d", code)
	}
}
