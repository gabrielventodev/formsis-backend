package webhooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxAttempts before a delivery is marked failed: retries after 1, 2, 4 … 64
	// minutes cover a bit over two hours of downtime on the receiver.
	MaxAttempts = 8
	batchSize   = 10
	// A claimed delivery is invisible to other workers for this long, so a
	// crash mid-send retries it instead of losing it.
	lease = 2 * time.Minute
)

func backoff(attempts int) time.Duration {
	return time.Minute << (attempts - 1)
}

// Worker posts queued deliveries. Several API replicas can run one each:
// deliveries are claimed with SKIP LOCKED.
type Worker struct {
	DB            *pgxpool.Pool
	AllowInsecure bool // development: plain HTTP and private addresses
	Client        *http.Client
	Poll          time.Duration

	kick chan struct{}
	once sync.Once
}

func NewWorker(db *pgxpool.Pool, allowInsecure bool) *Worker {
	return &Worker{DB: db, AllowInsecure: allowInsecure, Client: NewClient(allowInsecure), Poll: 5 * time.Second}
}

func (w *Worker) init() { w.once.Do(func() { w.kick = make(chan struct{}, 1) }) }

// Kick wakes the worker after a transaction queued deliveries. Safe on a nil Worker.
func (w *Worker) Kick() {
	if w == nil {
		return
	}
	w.init()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run sends deliveries until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	w.init()
	t := time.NewTicker(w.Poll)
	defer t.Stop()
	for {
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Warn("webhooks: run", "err", err)
		}
		if n == batchSize {
			continue // there may be more due
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

type claimed struct {
	id, event, payload, url, secret string
	attempts                        int
	active                          bool
}

// RunOnce sends one batch of due deliveries and returns how many it claimed.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	rows, err := w.DB.Query(ctx, `
		WITH due AS (
			SELECT id FROM webhook_deliveries
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE webhook_deliveries d SET next_attempt_at = now() + $2::interval
		FROM due, webhooks h
		WHERE d.id = due.id AND h.id = d.webhook_id
		RETURNING d.id, d.event, d.payload::text, d.attempts, h.url, h.secret, h.active`,
		batchSize, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return 0, err
	}
	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.event, &c.payload, &c.attempts, &c.url, &c.secret, &c.active); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var wg sync.WaitGroup
	for _, c := range batch {
		wg.Add(1)
		go func(c claimed) {
			defer wg.Done()
			code, sendErr := w.send(ctx, c)
			if err := w.record(ctx, c, code, sendErr); err != nil {
				slog.Warn("webhooks: record", "delivery", c.id, "err", err)
			}
		}(c)
	}
	wg.Wait()
	return len(batch), nil
}

// errDisabled marks deliveries of an endpoint switched off after they were queued.
var errDisabled = fmt.Errorf("el webhook está desactivado")

func (w *Worker) send(ctx context.Context, c claimed) (int, error) {
	if !c.active {
		return 0, errDisabled
	}
	// Re-check the stored URL: the rules may be stricter than when it was saved.
	if _, err := ValidateURL(c.url, w.AllowInsecure); err != nil {
		return 0, err
	}
	body := []byte(c.payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "FormFlow-Webhooks/1")
	req.Header.Set("FormFlow-Event", c.event)
	req.Header.Set("FormFlow-Delivery", c.id)
	req.Header.Set("FormFlow-Signature", Sign(c.secret, time.Now(), body))
	res, err := w.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := strings.TrimSpace(string(snippet))
		if msg == "" {
			msg = http.StatusText(res.StatusCode)
		}
		return res.StatusCode, fmt.Errorf("HTTP %d: %s", res.StatusCode, msg)
	}
	return res.StatusCode, nil
}

func (w *Worker) record(ctx context.Context, c claimed, code int, sendErr error) error {
	var status *int
	if code != 0 {
		status = &code
	}
	attempts := c.attempts + 1
	if sendErr == nil {
		_, err := w.DB.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'succeeded', attempts = $2, last_status_code = $3,
				last_error = '', last_attempt_at = now()
			WHERE id = $1`, c.id, attempts, status)
		return err
	}
	msg := sendErr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	final := attempts >= MaxAttempts || sendErr == errDisabled
	next := time.Now().Add(backoff(attempts))
	_, err := w.DB.Exec(ctx, `
		UPDATE webhook_deliveries SET
			status = CASE WHEN $2 THEN 'failed' ELSE 'pending' END,
			attempts = $3, last_status_code = $4, last_error = $5, last_attempt_at = now(), next_attempt_at = $6
		WHERE id = $1`, c.id, final, attempts, status, msg, next)
	return err
}
