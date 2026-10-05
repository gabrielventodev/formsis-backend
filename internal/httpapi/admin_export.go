package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/gabrielventodev/formflow/api/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// FileOpener reads stored uploads by their submission_files.storage_key.
// storage.Store (local disk or S3/MinIO) satisfies it.
type FileOpener interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var key, filename, mimeType string
	err := s.DB.QueryRow(r.Context(), `
		SELECT f.storage_key, f.filename, f.mime_type
		FROM submission_files f JOIN submissions s ON s.id = f.submission_id
		WHERE f.id::text = $1 AND s.id::text = $2 AND s.organization_id = $3`,
		chi.URLParam(r, "fileID"), chi.URLParam(r, "id"), id.OrgID,
	).Scan(&key, &filename, &mimeType)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Archivo no encontrado", nil)
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
	f, err := s.Files.Open(r.Context(), key)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "El archivo ya no está en el almacenamiento", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer f.Close()

	disposition := "attachment"
	if r.URL.Query().Get("inline") == "1" && previewable(mimeType) {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	_, _ = io.Copy(w, f)
}

func previewable(mimeType string) bool {
	return mimeType == "application/pdf" || (strings.HasPrefix(mimeType, "image/") && mimeType != "image/svg+xml")
}

// schemaField is the subset of a form field the export needs.
type schemaField struct {
	Key    string        `json:"key"`
	Label  string        `json:"label"`
	Type   string        `json:"type"`
	Fields []schemaField `json:"fields"`
}

type formSchema struct {
	Sections []struct {
		Key    string        `json:"key"`
		Fields []schemaField `json:"fields"`
	} `json:"sections"`
}

// exportSubmissions streams the filtered inbox as CSV. When filtered by one
// form, each top-level field of its current version becomes a column; otherwise
// the answers go in a single JSON column.
func (s *Server) exportSubmissions(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	where, args, err := submissionFilter(r, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	ctx := r.Context()

	var fields []schemaField
	if formID := r.URL.Query().Get("form_id"); formID != "" {
		var raw []byte
		err := s.DB.QueryRow(ctx, `
			SELECT COALESCE(
				(SELECT schema FROM form_versions WHERE id = f.current_version_id),
				(SELECT schema FROM form_versions WHERE form_id = f.id ORDER BY version_number DESC LIMIT 1),
				f.draft_schema)
			FROM forms f WHERE f.id::text = $1 AND f.organization_id = $2`, formID, id.OrgID).Scan(&raw)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.serverError(w, r, err)
			return
		}
		var sc formSchema
		if json.Unmarshal(raw, &sc) == nil {
			for _, sec := range sc.Sections {
				fields = append(fields, sec.Fields...)
			}
		}
	}

	rows, err := s.DB.Query(ctx, `
		SELECT s.id, f.title, v.version_number, s.applicant_email, s.applicant_name, s.status,
		       COALESCE(NULLIF(u.name, ''), u.email, ''), s.submitted_at, s.decided_at, s.data
		FROM submissions s
		JOIN forms f ON f.id = s.form_id
		JOIN form_versions v ON v.id = s.form_version_id
		LEFT JOIN users u ON u.id = s.assigned_to`+where+submissionOrder(r), args...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="envios-%s.csv"`, time.Now().Format("2006-01-02")))
	_, _ = w.Write([]byte("\ufeff")) // BOM so Excel opens accents correctly

	cw := csv.NewWriter(w)
	header := []string{"id", "formulario", "versión", "email", "nombre", "estado", "revisor", "enviado", "decidido"}
	if fields != nil {
		for _, f := range fields {
			header = append(header, labelOr(f))
		}
	} else {
		header = append(header, "respuestas")
	}
	_ = cw.Write(header)

	for rows.Next() {
		var (
			subID, title, email, name, status, reviewer string
			version                                     int
			submitted, decided                          *time.Time
			data                                        []byte
		)
		if err := rows.Scan(&subID, &title, &version, &email, &name, &status, &reviewer, &submitted, &decided, &data); err != nil {
			return // headers already sent; truncated CSV is the best we can do
		}
		rec := []string{subID, title, fmt.Sprint(version), email, name, statusLabel(status), reviewer, fmtTime(submitted), fmtTime(decided)}
		if fields != nil {
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			for _, f := range fields {
				rec = append(rec, csvValue(lookupAnswer(m, f.Key)))
			}
		} else {
			rec = append(rec, string(data))
		}
		for i := range rec {
			rec[i] = csvSafe(rec[i])
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// csvSafe neutralizes spreadsheet formulas in applicant-provided text.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func labelOr(f schemaField) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Key
}

// lookupAnswer finds a field value whether answers are stored flat or grouped by section.
func lookupAnswer(data map[string]any, key string) any {
	if v, ok := data[key]; ok {
		return v
	}
	for _, v := range data {
		if sec, ok := v.(map[string]any); ok {
			if inner, ok := sec[key]; ok {
				return inner
			}
		}
	}
	return nil
}

func csvValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "Sí"
		}
		return "No"
	case []any:
		parts := make([]string, 0, len(t))
		simple := true
		for _, e := range t {
			if _, ok := e.(map[string]any); ok {
				simple = false
				break
			}
			parts = append(parts, csvValue(e))
		}
		if simple {
			return strings.Join(parts, "; ")
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04")
}

var statusLabels = map[string]string{
	"draft":             "Borrador",
	"submitted":         "Enviado",
	"in_review":         "En revisión",
	"changes_requested": "Observado",
	"approved":          "Aprobado",
	"rejected":          "Rechazado",
}

func statusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}
