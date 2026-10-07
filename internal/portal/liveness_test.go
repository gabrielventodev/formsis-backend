package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/gabrielventodev/formsis/api/internal/face"
	"github.com/jackc/pgx/v5/pgxpool"
)

const livenessSchema = `{"sections":[{"key":"a","title":"A","fields":[
  {"key":"nombre","type":"text","label":"Nombre"},
  {"key":"vida","type":"liveness","label":"Verifica que eres tú","required":true}
]}]}`

// A JPEG header is enough: the fake face service doesn't decode images.
var jpegFrame = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{1}, 64)...)

// fakeFace answers every liveness request with the next queued decision and records requests.
type fakeFace struct {
	mu        sync.Mutex
	decisions []string
	calls     int
	lastSteps string
	lastFrame int
}

func (f *fakeFace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer secreto" {
		http.Error(w, "unauthorized", 401)
		return
	}
	_ = r.ParseMultipartForm(32 << 20)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSteps = r.FormValue("steps")
	f.lastFrame = len(r.MultipartForm.File["frames"])
	d := "pass"
	if len(f.decisions) > 0 {
		d, f.decisions = f.decisions[0], f.decisions[1:]
	}
	if d == "boom" {
		http.Error(w, "kaput", 500)
		return
	}
	reasons := []string{}
	if d == "retry" {
		reasons = []string{"too_dark"}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"decision": d, "reasons": reasons, "best_frame": 0, "scores": map[string]any{"passive": 0.97}})
}

func livenessSetup(t *testing.T, ff *fakeFace) (*httptest.Server, *pgxpool.Pool, string) {
	t.Helper()
	var faceURL string
	if ff != nil {
		fs := httptest.NewServer(ff)
		t.Cleanup(fs.Close)
		faceURL = fs.URL
	}
	srv, pool, link := setupWith(t, livenessSchema, func(h *Handler) { h.Face = face.New(faceURL, "secreto") })
	code, body := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "ana@empresa.cl"})
	if code != 201 {
		t.Fatalf("start: %d %v", code, body)
	}
	return srv, pool, body["accessToken"].(string)
}

func sendFrames(t *testing.T, srv *httptest.Server, token, checkID string, frameSteps []int, frame []byte) (int, map[string]any) {
	t.Helper()
	return postFrames(t, srv, "/submission/liveness/"+checkID, token, frameSteps, frame)
}

func postFrames(t *testing.T, srv *httptest.Server, path, token string, frameSteps []int, frame []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fs, _ := json.Marshal(frameSteps)
	_ = mw.WriteField("frameSteps", string(fs))
	for i := range frameSteps {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="frames"; filename="%d.jpg"`, i))
		h.Set("Content-Type", "image/jpeg")
		w, _ := mw.CreatePart(h)
		_, _ = w.Write(frame)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func challenge(t *testing.T, srv *httptest.Server, tok string) map[string]any {
	t.Helper()
	code, body := call(t, srv, "POST", "/submission/liveness", tok, map[string]string{"fieldKey": "vida"})
	if code != 201 {
		t.Fatalf("challenge: %d %v", code, body)
	}
	return body
}

func TestLivenessFlow(t *testing.T) {
	ff := &fakeFace{decisions: []string{"retry", "pass"}}
	srv, _, tok := livenessSetup(t, ff)

	// The field is required: submitting without a completed check fails.
	code, body := call(t, srv, "POST", "/submission/submit", tok, nil)
	if code != 422 || body["errors"].(map[string]any)["vida"] == nil {
		t.Fatalf("submit without liveness: %d %v", code, body)
	}

	// Only liveness fields get challenges.
	if code, _ := call(t, srv, "POST", "/submission/liveness", tok, map[string]string{"fieldKey": "nombre"}); code != 400 {
		t.Fatalf("challenge for text field: %d", code)
	}

	ch := challenge(t, srv, tok)
	steps := ch["steps"].([]any)
	if len(steps) != 3 || steps[0] != "center" || steps[1] == steps[2] {
		t.Fatalf("steps: %v", steps)
	}
	if ch["attemptsLeft"].(float64) != 4 || ch["framesPerStep"].(float64) != 3 {
		t.Fatalf("challenge: %v", ch)
	}
	id := ch["id"].(string)

	// Frames must be images and belong to the challenge's steps.
	if code, _ := sendFrames(t, srv, tok, id, []int{0, 1}, []byte("hola")); code != 400 {
		t.Fatalf("non-image frames: %d", code)
	}
	if code, _ := sendFrames(t, srv, tok, id, []int{0, 7}, jpegFrame); code != 400 {
		t.Fatalf("frame for unknown step: %d", code)
	}

	// First attempt: the service asks to retry.
	code, body = sendFrames(t, srv, tok, id, []int{0, 0, 1, 1, 2, 2}, jpegFrame)
	if code != 200 || body["decision"] != "retry" || body["completed"] != false {
		t.Fatalf("retry attempt: %d %v", code, body)
	}
	if ff.lastFrame != 6 || !strings.HasPrefix(ff.lastSteps, `["center",`) {
		t.Fatalf("service got %d frames, steps %s", ff.lastFrame, ff.lastSteps)
	}
	// An attempt can be answered only once.
	if code, _ := sendFrames(t, srv, tok, id, []int{0}, jpegFrame); code != 404 {
		t.Fatalf("second answer: %d", code)
	}

	// Second attempt passes and the form can be submitted.
	ch = challenge(t, srv, tok)
	code, body = sendFrames(t, srv, tok, ch["id"].(string), []int{0, 1, 2}, jpegFrame)
	if code != 200 || body["decision"] != "pass" || body["completed"] != true || body["attemptsLeft"].(float64) != 3 {
		t.Fatalf("pass attempt: %d %v", code, body)
	}
	_, body = call(t, srv, "GET", "/submission", tok, nil)
	if l := body["liveness"].([]any); len(l) != 2 || l[1].(map[string]any)["decision"] != "pass" {
		t.Fatalf("liveness list: %v", body["liveness"])
	}
	if code, body := call(t, srv, "POST", "/submission/submit", tok, nil); code != 200 {
		t.Fatalf("submit: %d %v", code, body)
	}
	// After submitting, the field is locked.
	if code, _ := call(t, srv, "POST", "/submission/liveness", tok, map[string]string{"fieldKey": "vida"}); code != 409 {
		t.Fatalf("challenge after submit: %d", code)
	}
}

func TestLivenessLimitsAndErrors(t *testing.T) {
	ff := &fakeFace{decisions: []string{"boom"}}
	srv, pool, tok := livenessSetup(t, ff)

	// A service failure is reported and recorded, not counted as completed.
	ch := challenge(t, srv, tok)
	if code, _ := sendFrames(t, srv, tok, ch["id"].(string), []int{0}, jpegFrame); code != 503 {
		t.Fatalf("service down: %d", code)
	}

	// Expired challenges are refused.
	ch = challenge(t, srv, tok)
	_, err := pool.Exec(context.Background(), `UPDATE liveness_checks SET expires_at = now() - interval '1 second' WHERE id = $1`, ch["id"])
	must(t, err)
	if code, _ := sendFrames(t, srv, tok, ch["id"].(string), []int{0}, jpegFrame); code != 410 {
		t.Fatalf("expired: %d", code)
	}

	// Five challenges per field; then the applicant has to contact the organization.
	for i := 0; i < 3; i++ {
		challenge(t, srv, tok)
	}
	if code, _ := call(t, srv, "POST", "/submission/liveness", tok, map[string]string{"fieldKey": "vida"}); code != 429 {
		t.Fatalf("sixth challenge: %d", code)
	}
}

func TestLivenessUnavailableWithoutService(t *testing.T) {
	srv, _, tok := livenessSetup(t, nil)
	if code, _ := call(t, srv, "POST", "/submission/liveness", tok, map[string]string{"fieldKey": "vida"}); code != 503 {
		t.Fatalf("no face service: %d", code)
	}
}
