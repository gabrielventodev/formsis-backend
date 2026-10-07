package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/gabrielventodev/formflow/api/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// livenessRow is one liveness attempt as the reviewer sees it. Result is the face service's full
// answer (per-frame measurements, scores and model versions); frames are served one by one.
type livenessRow struct {
	ID          string          `json:"id"`
	FieldKey    string          `json:"field_key"`
	Steps       []string        `json:"steps"`
	Decision    *string         `json:"decision"`
	Reasons     []string        `json:"reasons"`
	Result      json.RawMessage `json:"result"`
	FrameSteps  []int           `json:"frame_steps"`
	BestFrame   *int            `json:"best_frame"`
	CreatedAt   time.Time       `json:"created_at"`
	CompletedAt *time.Time      `json:"completed_at"`
	// Taken on a phone through the QR shown on a computer.
	FromPhone bool `json:"from_phone"`
}

type storedFrame struct {
	Step        int    `json:"step"`
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
}

// loadLiveness returns every finished attempt of a submission, oldest first.
func (s *Server) loadLiveness(ctx context.Context, submissionID string) ([]livenessRow, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, field_key, steps, decision, reasons, result, frames, best_frame, created_at, completed_at,
		       handoff_id IS NOT NULL
		FROM liveness_checks WHERE submission_id = $1 AND decision IS NOT NULL ORDER BY created_at`, submissionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (livenessRow, error) {
		var l livenessRow
		var frames []storedFrame
		if err := row.Scan(&l.ID, &l.FieldKey, &l.Steps, &l.Decision, &l.Reasons, &l.Result, &frames,
			&l.BestFrame, &l.CreatedAt, &l.CompletedAt, &l.FromPhone); err != nil {
			return l, err
		}
		l.FrameSteps = make([]int, len(frames))
		for i, f := range frames {
			l.FrameSteps[i] = f.Step
		}
		return l, nil
	})
}

// livenessFrame serves one stored frame of an attempt as an image.
func (s *Server) livenessFrame(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 0 {
		writeError(w, http.StatusNotFound, "Imagen no encontrada", nil)
		return
	}
	var frames []storedFrame
	err = s.DB.QueryRow(r.Context(), `
		SELECT c.frames FROM liveness_checks c JOIN submissions s ON s.id = c.submission_id
		WHERE c.id::text = $1 AND s.id::text = $2 AND s.organization_id = $3`,
		chi.URLParam(r, "checkID"), chi.URLParam(r, "id"), id.OrgID).Scan(&frames)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && n >= len(frames)) {
		writeError(w, http.StatusNotFound, "Imagen no encontrada", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "Almacenamiento de archivos no configurado", nil)
		return
	}
	f, err := s.Files.Open(r.Context(), frames[n].Key)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "La imagen ya no está en el almacenamiento", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer f.Close()
	ct := frames[n].ContentType
	if ct != "image/png" {
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, f)
}
