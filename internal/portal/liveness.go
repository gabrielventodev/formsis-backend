package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/face"
	"github.com/gabrielventodev/formsis/api/internal/schema"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Liveness: the applicant asks for a challenge, the browser shows each step for StepMs and takes
// FramesPerStep frames during it, then posts them all at once. The face service decides; every
// attempt is stored with its frames so a reviewer can look at it.
const (
	livenessStepMs        = 2500
	livenessFramesPerStep = 3
	// Time to show the whole challenge and upload it; a short window makes it harder to
	// prepare a fake video for the steps that were asked.
	livenessTTL         = 90 * time.Second
	livenessMaxAttempts = 5
	livenessMaxFrames   = 20
	livenessMaxFrameMB  = 1.5
)

type livenessOut struct {
	ID          string     `json:"id"`
	FieldKey    string     `json:"fieldKey"`
	Decision    string     `json:"decision"`
	Reasons     []string   `json:"reasons"`
	CompletedAt *time.Time `json:"completedAt"`
}

// listLiveness returns the finished attempts of a submission, oldest first.
func (h *Handler) listLiveness(ctx context.Context, subID string) ([]livenessOut, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT id, field_key, decision, reasons, completed_at FROM liveness_checks
		WHERE submission_id = $1 AND decision IS NOT NULL ORDER BY created_at`, subID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (livenessOut, error) {
		var l livenessOut
		err := row.Scan(&l.ID, &l.FieldKey, &l.Decision, &l.Reasons, &l.CompletedAt)
		return l, err
	})
}

// livenessField checks that the applicant may (re)do the liveness check of a field now.
func (h *Handler) livenessField(ctx context.Context, s *submission, fieldKey string) (string, int, error) {
	f := s.Schema.Find(fieldKey)
	if f == nil || f.Type != schema.TypeLiveness || strings.Contains(fieldKey, ".") {
		return "Campo de verificación no válido.", http.StatusBadRequest, nil
	}
	var comments []commentOut
	if s.Status == "changes_requested" {
		var err error
		if comments, err = h.openComments(ctx, s.ID); err != nil {
			return "", 0, err
		}
	}
	keys, ok := editable(s.Status, comments)
	if !ok || (keys != nil && !keys[fieldKey]) {
		return "Este campo no se puede modificar.", http.StatusConflict, nil
	}
	if h.Face == nil {
		return "La verificación con cámara no está disponible en este momento.", http.StatusServiceUnavailable, nil
	}
	return "", 0, nil
}

// attemptsUsed counts the challenges issued for a field since the applicant last submitted, so
// a reviewer asking for changes gives them a fresh set of attempts.
func (h *Handler) attemptsUsed(ctx context.Context, subID, fieldKey string) (int, error) {
	var n int
	err := h.DB.QueryRow(ctx, `
		SELECT count(*) FROM liveness_checks c JOIN submissions s ON s.id = c.submission_id
		WHERE c.submission_id = $1 AND c.field_key = $2
		  AND c.created_at > COALESCE(s.submitted_at, '-infinity'::timestamptz)`, subID, fieldKey).Scan(&n)
	return n, err
}

type livenessStartRequest struct {
	FieldKey string `json:"fieldKey"`
}

func (h *Handler) startLiveness(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	var req livenessStartRequest
	// From a phone, the handoff fixes the field.
	ho := currentHandoff(r)
	if ho != nil {
		req.FieldKey = ho.FieldKey
	} else if err := decode(r, &req, 1<<12); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud no válida.")
		return
	}
	msg, status, err := h.livenessField(ctx, s, req.FieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	used, err := h.attemptsUsed(ctx, s.ID, req.FieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if used >= livenessMaxAttempts {
		writeError(w, http.StatusTooManyRequests, "Llegaste al máximo de intentos. Escríbenos para que revisemos tu caso.")
		return
	}
	steps := face.NewChallenge()
	var id string
	var expiresAt time.Time
	if err := h.DB.QueryRow(ctx, `
		INSERT INTO liveness_checks (submission_id, field_key, steps, expires_at, handoff_id)
		VALUES ($1, $2, $3, now() + $4::interval, $5)
		RETURNING id, expires_at`, s.ID, req.FieldKey, steps, fmt.Sprintf("%d seconds", int(livenessTTL.Seconds())), ho.id()).
		Scan(&id, &expiresAt); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":            id,
		"steps":         steps,
		"stepMs":        livenessStepMs,
		"framesPerStep": livenessFramesPerStep,
		"expiresAt":     expiresAt,
		"attemptsLeft":  livenessMaxAttempts - used - 1,
	})
}

func (h *Handler) finishLiveness(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	ho := currentHandoff(r)
	checkID := chi.URLParam(r, "id")

	r.Body = http.MaxBytesReader(w, r.Body, int64(livenessMaxFrames*livenessMaxFrameMB*(1<<20))+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "Las imágenes son demasiado grandes.")
		return
	}
	defer r.MultipartForm.RemoveAll()

	var fieldKey string
	var steps []string
	var expiresAt time.Time
	err := h.DB.QueryRow(ctx, `
		SELECT field_key, steps, expires_at FROM liveness_checks
		WHERE id = $1 AND submission_id = $2 AND decision IS NULL
		  AND ($3::uuid IS NULL OR handoff_id = $3)`, checkID, s.ID, ho.id()).Scan(&fieldKey, &steps, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "Este intento ya terminó. Empieza uno nuevo.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	msg, status, err := h.livenessField(ctx, s, fieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if msg != "" {
		writeError(w, status, msg)
		return
	}

	frames, problem := readFrames(r, len(steps))
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}

	// Claim the attempt so it can't be answered twice.
	tag, err := h.DB.Exec(ctx, `
		UPDATE liveness_checks SET decision = CASE WHEN expires_at < now() THEN 'expired' ELSE 'error' END,
		       completed_at = now()
		WHERE id = $1 AND decision IS NULL`, checkID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "Este intento ya terminó. Empieza uno nuevo.")
		return
	}
	if time.Now().After(expiresAt) {
		writeError(w, http.StatusGone, "Se acabó el tiempo de este intento. Empieza uno nuevo.")
		return
	}

	res, ferr := h.Face.Liveness(ctx, steps, frames)
	if ferr != nil {
		slog.Error("liveness", "check", checkID, "err", ferr)
	}

	stored := make([]map[string]any, 0, len(frames))
	for i, f := range frames {
		key := fmt.Sprintf("%s/submissions/%s/liveness/%s/%02d.jpg", s.OrgID, s.ID, checkID, i)
		if err := h.Store.Put(ctx, key, bytes.NewReader(f.Data), int64(len(f.Data)), f.ContentType); err != nil {
			serverError(w, r, err)
			return
		}
		stored = append(stored, map[string]any{"step": f.Step, "key": key, "contentType": f.ContentType})
	}

	decision, reasons, raw := "error", []string{}, json.RawMessage(nil)
	if ferr == nil {
		decision, reasons, raw = res.Decision, res.Reasons, res.Raw
	}
	err = pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE liveness_checks SET decision = $2, reasons = $3, result = $4, frames = $5, best_frame = $6
			WHERE id = $1`, checkID, decision, reasons, raw, stored, res.BestFrame); err != nil {
			return err
		}
		// A phone link is single-use once the field is done.
		if ho != nil && face.Completed(decision) {
			if _, err := tx.Exec(ctx, `UPDATE liveness_handoffs SET expires_at = least(expires_at, now()) WHERE id = $1`, ho.ID); err != nil {
				return err
			}
		}
		return audit(ctx, tx, s.ID, s.Email, "liveness.completed", nil, nil, map[string]any{
			"check_id": checkID, "field_key": fieldKey, "decision": decision, "reasons": reasons, "from_phone": ho != nil,
		})
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	if ferr != nil {
		writeError(w, http.StatusServiceUnavailable, "No pudimos verificar ahora. Intenta de nuevo en un momento.")
		return
	}
	used, err := h.attemptsUsed(ctx, s.ID, fieldKey)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           checkID,
		"fieldKey":     fieldKey,
		"decision":     decision,
		"reasons":      reasons,
		"completed":    face.Completed(decision),
		"attemptsLeft": max(livenessMaxAttempts-used, 0),
	})
}

// readFrames reads the "frames" files and the "frameSteps" JSON list (the step index of each).
func readFrames(r *http.Request, nSteps int) ([]face.Frame, string) {
	files := r.MultipartForm.File["frames"]
	var frameSteps []int
	if err := json.Unmarshal([]byte(r.FormValue("frameSteps")), &frameSteps); err != nil || len(frameSteps) != len(files) {
		return nil, "Faltan las imágenes de la verificación."
	}
	if len(files) == 0 || len(files) > livenessMaxFrames {
		return nil, fmt.Sprintf("Envía entre 1 y %d imágenes.", livenessMaxFrames)
	}
	frames := make([]face.Frame, 0, len(files))
	for i, fh := range files {
		if frameSteps[i] < 0 || frameSteps[i] >= nSteps {
			return nil, "Las imágenes no corresponden a este intento."
		}
		if float64(fh.Size) > livenessMaxFrameMB*(1<<20) {
			return nil, "Las imágenes son demasiado grandes."
		}
		f, err := fh.Open()
		if err != nil {
			return nil, "No pudimos leer las imágenes."
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, "No pudimos leer las imágenes."
		}
		ct := http.DetectContentType(data)
		if ct != "image/jpeg" && ct != "image/png" {
			return nil, "Las imágenes deben ser JPEG o PNG."
		}
		frames = append(frames, face.Frame{Step: frameSteps[i], Data: data, ContentType: ct})
	}
	return frames, ""
}
