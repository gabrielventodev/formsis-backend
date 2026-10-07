package portal

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/gabrielventodev/formsis/api/internal/schema"
	"github.com/gabrielventodev/formsis/api/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

var fieldPathRe = regexp.MustCompile(`^[A-Za-z0-9_\-]+(\.[0-9]+\.[A-Za-z0-9_\-]+)*$`)

// Types the browser may render inline; anything else is served as a download.
var inlineTypes = map[string]bool{
	"application/pdf": true, "image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

func (h *Handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)

	limitMB := h.MaxUploadMB
	if limitMB <= 0 {
		limitMB = 25
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(limitMB*(1<<20))+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("El archivo supera el máximo de %s MB.", trimFloat(limitMB)))
		return
	}
	defer r.MultipartForm.RemoveAll()

	fieldKey := r.FormValue("fieldKey")
	field := s.Schema.Find(fieldKey)
	if !fieldPathRe.MatchString(fieldKey) || field == nil || field.Type != schema.TypeFile {
		writeError(w, http.StatusBadRequest, "Campo de archivo no válido.")
		return
	}
	var comments []commentOut
	var err error
	if s.Status == "changes_requested" {
		if comments, err = h.openComments(ctx, s.ID); err != nil {
			serverError(w, r, err)
			return
		}
	}
	keys, ok := editable(s.Status, comments)
	if !ok || (keys != nil && !keys[rootKey(fieldKey)]) {
		writeError(w, http.StatusConflict, "Este campo no se puede modificar.")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Adjunta un archivo.")
		return
	}
	defer file.Close()

	if field.MaxMb != nil && float64(header.Size) > *field.MaxMb*(1<<20) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("El archivo supera el máximo de %s MB.", trimFloat(*field.MaxMb)))
		return
	}
	if float64(header.Size) > limitMB*(1<<20) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("El archivo supera el máximo de %s MB.", trimFloat(limitMB)))
		return
	}

	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	sniffed := strings.TrimSpace(strings.Split(http.DetectContentType(head[:n]), ";")[0])
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		serverError(w, r, err)
		return
	}
	declared, _, _ := mime.ParseMediaType(header.Header.Get("Content-Type"))
	filename := cleanFilename(header.Filename)
	mimeType := sniffed
	if mimeType == "application/octet-stream" || mimeType == "application/zip" || strings.HasPrefix(mimeType, "text/plain") {
		if declared != "" && !strings.Contains(declared, "html") && !strings.Contains(declared, "svg") {
			mimeType = declared // docx, xlsx and csv sniff as zip/text; keep what the browser said
		}
	}
	if !accepts(field.Accept, filename, mimeType) {
		writeError(w, http.StatusUnsupportedMediaType, "Tipo de archivo no permitido. Formatos aceptados: "+strings.Join(field.Accept, ", ")+".")
		return
	}

	var out fileOut
	err = pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil {
			return err
		}
		key := fmt.Sprintf("%s/submissions/%s/%s%s", s.OrgID, s.ID, id, strings.ToLower(path.Ext(filename)))
		if err := tx.QueryRow(ctx, `
			INSERT INTO submission_files (id, submission_id, field_key, storage_key, filename, mime_type, size_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id, field_key, filename, mime_type, size_bytes, uploaded_at`,
			id, s.ID, fieldKey, key, filename, mimeType, header.Size).
			Scan(&out.ID, &out.FieldKey, &out.Filename, &out.MimeType, &out.SizeBytes, &out.UploadedAt); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.ID, s.Email, "file.uploaded", nil, nil,
			map[string]any{"file_id": id, "field_key": fieldKey, "filename": filename, "size_bytes": header.Size}); err != nil {
			return err
		}
		// Upload last: if it fails the row is rolled back; if the commit fails we only leave an orphan object.
		return h.Store.Put(ctx, key, file, header.Size, mimeType)
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	var key, filename, mimeType string
	err := h.DB.QueryRow(ctx, `
		SELECT storage_key, filename, mime_type FROM submission_files WHERE id = $1 AND submission_id = $2`,
		chi.URLParam(r, "id"), s.ID).Scan(&key, &filename, &mimeType)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "Archivo no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	rc, err := h.Store.Open(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Archivo no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rc.Close()
	ServeStored(w, rc, filename, mimeType)
}

// ServeStored writes an uploaded document safely: never as active content, inline only for PDFs and images.
func ServeStored(w http.ResponseWriter, rc io.Reader, filename, mimeType string) {
	disposition := "attachment"
	if inlineTypes[mimeType] {
		disposition = "inline"
	} else {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, rc)
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := current(r)
	var key, fieldKey string
	err := h.DB.QueryRow(ctx, `
		SELECT storage_key, field_key FROM submission_files WHERE id = $1 AND submission_id = $2`,
		chi.URLParam(r, "id"), s.ID).Scan(&key, &fieldKey)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "Archivo no encontrado.")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	var comments []commentOut
	if s.Status == "changes_requested" {
		if comments, err = h.openComments(ctx, s.ID); err != nil {
			serverError(w, r, err)
			return
		}
	}
	keys, ok := editable(s.Status, comments)
	if !ok || (keys != nil && !keys[rootKey(fieldKey)]) {
		writeError(w, http.StatusConflict, "Este archivo no se puede eliminar.")
		return
	}
	err = pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM submission_files WHERE id = $1`, chi.URLParam(r, "id")); err != nil {
			return err
		}
		return audit(ctx, tx, s.ID, s.Email, "file.deleted", nil, nil,
			map[string]any{"file_id": chi.URLParam(r, "id"), "field_key": fieldKey})
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	if err := h.Store.Delete(ctx, key); err != nil {
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// accepts matches MIME types ("application/pdf", "image/*") or extensions (".pdf"). Empty = anything.
func accepts(accept []string, filename, mimeType string) bool {
	if len(accept) == 0 {
		return true
	}
	ext := strings.ToLower(path.Ext(filename))
	for _, a := range accept {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case strings.HasPrefix(a, "."):
			if a == ext {
				return true
			}
		case strings.HasSuffix(a, "/*"):
			if strings.HasPrefix(mimeType, strings.TrimSuffix(a, "*")) {
				return true
			}
		case a == mimeType:
			return true
		}
	}
	return false
}

func cleanFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" {
		name = "archivo"
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}

func isInvalidUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}

func trimFloat(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}
