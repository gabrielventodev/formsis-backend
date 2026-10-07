// Package face talks to the formsis-face service, which checks that a live person is in front
// of the camera (liveness). The service is stateless and only reachable on the internal network;
// this package sends it the frames of an attempt and returns its decision.
package face

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// Decisions returned by the service. Pass and Review complete the field; Retry and Fail ask the
// applicant to try again.
const (
	Pass   = "pass"
	Review = "review"
	Retry  = "retry"
	Fail   = "fail"
)

// Completed reports whether a decision satisfies a liveness field.
func Completed(decision string) bool { return decision == Pass || decision == Review }

// Challenge steps the service understands. Every challenge starts frontal ("center").
var optionalSteps = []string{"left", "right", "closer"}

// NewChallenge returns "center" followed by two different steps in random order, so a
// pre-recorded video has one chance in six of matching.
func NewChallenge() []string {
	steps := append([]string(nil), optionalSteps...)
	rand.Shuffle(len(steps), func(i, j int) { steps[i], steps[j] = steps[j], steps[i] })
	return append([]string{"center"}, steps[:2]...)
}

// Frame is one camera frame and the index of the challenge step shown when it was taken.
type Frame struct {
	Step        int
	Data        []byte
	ContentType string
}

// Result is the service's answer. Raw keeps the whole response (per-frame measurements, scores
// and model versions) for the reviewer and for calibrating thresholds later.
type Result struct {
	Decision  string   `json:"decision"`
	Reasons   []string `json:"reasons"`
	BestFrame *int     `json:"best_frame"`
	Raw       json.RawMessage
}

type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// New returns nil when no URL is configured: liveness fields then answer 503.
func New(url, token string) *Client {
	if url == "" {
		return nil
	}
	return &Client{URL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// ErrUnavailable means the service could not give a decision (down, overloaded or misconfigured).
var ErrUnavailable = errors.New("face service unavailable")

func (c *Client) Liveness(ctx context.Context, steps []string, frames []Frame) (Result, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	stepsJSON, _ := json.Marshal(steps)
	frameSteps := make([]int, len(frames))
	for i, f := range frames {
		frameSteps[i] = f.Step
	}
	frameStepsJSON, _ := json.Marshal(frameSteps)
	_ = mw.WriteField("steps", string(stepsJSON))
	_ = mw.WriteField("frame_steps", string(frameStepsJSON))
	for i, f := range frames {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="frames"; filename="%d.jpg"`, i))
		h.Set("Content-Type", f.ContentType)
		part, err := mw.CreatePart(h)
		if err != nil {
			return Result{}, err
		}
		if _, err := part.Write(f.Data); err != nil {
			return Result{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/liveness", &body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, truncate(string(raw), 300))
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, fmt.Errorf("%w: bad response: %v", ErrUnavailable, err)
	}
	switch res.Decision {
	case Pass, Review, Retry, Fail:
	default:
		return Result{}, fmt.Errorf("%w: unknown decision %q", ErrUnavailable, res.Decision)
	}
	if res.Reasons == nil {
		res.Reasons = []string{}
	}
	res.Raw = raw
	return res, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
