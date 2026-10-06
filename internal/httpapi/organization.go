package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/gabrielventodev/formflow/api/internal/branding"
	"github.com/jackc/pgx/v5"
)

// Organization settings and branding: the name, primary color, logo and
// support email the applicant portal and the emails show.

const maxLogoBytes = 1 << 20

// Raster formats only: an SVG logo could carry scripts.
var logoTypes = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}

type brandingOut struct {
	Name         string  `json:"name"`
	PrimaryColor string  `json:"primary_color"`
	SupportEmail string  `json:"support_email"`
	LogoURL      *string `json:"logo_url"`
}

func brandingView(b branding.Brand) brandingOut {
	out := brandingOut{Name: b.Name, PrimaryColor: b.Color(), SupportEmail: b.SupportEmail}
	if p := b.LogoPath(); p != "" {
		out.LogoURL = &p
	}
	return out
}

// publicBranding is read by the applicant portal (no session).
func (s *Server) publicBranding(w http.ResponseWriter, r *http.Request) {
	b, err := branding.Load(r.Context(), s.DB, s.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, brandingView(b))
}

func (s *Server) publicLogo(w http.ResponseWriter, r *http.Request) {
	b, err := branding.Load(r.Context(), s.DB, s.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if b.LogoKey == "" || s.Uploads == nil {
		writeError(w, http.StatusNotFound, "Sin logo", nil)
		return
	}
	rc, err := s.Uploads.Open(r.Context(), b.LogoKey)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", b.LogoMime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "public, max-age=86400") // URLs carry ?v=<version>
	_, _ = io.Copy(w, rc)
}

func (s *Server) getOrganization(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	b, err := branding.Load(r.Context(), s.DB, id.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, brandingView(b))
}

// updateOrganization changes name, color and support email (the logo has its own endpoints).
func (s *Server) updateOrganization(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		Name         *string `json:"name"`
		PrimaryColor *string `json:"primary_color"`
		SupportEmail *string `json:"support_email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	err := s.editBranding(r.Context(), id, func(b *branding.Brand) (map[string]any, error) {
		meta := map[string]any{}
		if in.Name != nil {
			name := strings.TrimSpace(*in.Name)
			if name == "" || utf8.RuneCountInString(name) > 120 {
				return nil, badRequest("El nombre debe tener entre 1 y 120 caracteres")
			}
			if name != b.Name {
				b.Name, meta["name"] = name, name
			}
		}
		if in.PrimaryColor != nil {
			c, err := branding.CheckColor(strings.TrimSpace(*in.PrimaryColor))
			if errors.Is(err, branding.ErrLowContrast) {
				return nil, badRequest("Ese color es muy claro: el texto blanco de los botones no se leería. Elige uno más oscuro.")
			}
			if err != nil {
				return nil, badRequest("Color inválido: usa el formato #RRGGBB")
			}
			if c != b.Color() {
				b.PrimaryColor, meta["primary_color"] = c, c
			}
		}
		if in.SupportEmail != nil {
			e := strings.ToLower(strings.TrimSpace(*in.SupportEmail))
			if e != "" {
				if a, err := mail.ParseAddress(e); err != nil || a.Address != e {
					return nil, badRequest("Email de contacto inválido")
				}
			}
			if e != b.SupportEmail {
				b.SupportEmail, meta["support_email"] = e, e
			}
		}
		return meta, nil
	})
	s.respondBranding(w, r, id, err)
}

func (s *Server) uploadLogo(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if s.Uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "El almacenamiento de archivos no está configurado", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLogoBytes+64<<10)
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Adjunta una imagen PNG, JPG o WebP de hasta 1 MB", nil)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLogoBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "No se pudo leer la imagen", nil)
		return
	}
	if len(data) > maxLogoBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "El logo supera 1 MB", nil)
		return
	}
	mimeType := strings.Split(http.DetectContentType(data), ";")[0]
	ext, ok := logoTypes[mimeType]
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "Usa una imagen PNG, JPG o WebP", nil)
		return
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	key := id.OrgID + "/branding/logo-" + hex.EncodeToString(suffix) + "." + ext
	if err := s.Uploads.Put(r.Context(), key, bytes.NewReader(data), int64(len(data)), mimeType); err != nil {
		s.serverError(w, r, err)
		return
	}
	var old string
	err = s.editBranding(r.Context(), id, func(b *branding.Brand) (map[string]any, error) {
		old = b.LogoKey
		b.LogoKey, b.LogoMime = key, mimeType
		b.LogoVersion++
		return map[string]any{"logo": "updated"}, nil
	})
	if err == nil && old != "" {
		s.deleteStored(r.Context(), old)
	}
	s.respondBranding(w, r, id, err)
}

func (s *Server) deleteLogo(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var old string
	err := s.editBranding(r.Context(), id, func(b *branding.Brand) (map[string]any, error) {
		if b.LogoKey == "" {
			return map[string]any{}, nil
		}
		old = b.LogoKey
		b.LogoKey, b.LogoMime = "", ""
		b.LogoVersion++
		return map[string]any{"logo": "removed"}, nil
	})
	if err == nil && old != "" {
		s.deleteStored(r.Context(), old)
	}
	s.respondBranding(w, r, id, err)
}

func (s *Server) deleteStored(ctx context.Context, key string) {
	if err := s.Uploads.Delete(ctx, key); err != nil {
		slog.Warn("delete old logo", "key", key, "err", err)
	}
}

type badRequest string

func (e badRequest) Error() string { return string(e) }

// editBranding loads the organization's branding under a row lock, lets
// change apply edits (returning audit metadata; empty means nothing changed)
// and saves it.
func (s *Server) editBranding(ctx context.Context, id auth.Identity, change func(*branding.Brand) (map[string]any, error)) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM organizations WHERE id = $1 FOR UPDATE`, id.OrgID); err != nil {
			return err
		}
		b, err := branding.Load(ctx, tx, id.OrgID)
		if err != nil {
			return err
		}
		meta, err := change(&b)
		if err != nil || len(meta) == 0 {
			return err
		}
		raw, err := json.Marshal(b.Settings)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET name = $2, branding = $3 WHERE id = $1`, id.OrgID, b.Name, raw); err != nil {
			return err
		}
		return insertOrgAudit(ctx, tx, id.OrgID, id.UserID, "organization.updated", meta)
	})
}

func (s *Server) respondBranding(w http.ResponseWriter, r *http.Request, id auth.Identity, err error) {
	var bad badRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, string(bad), nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.getOrganization(w, r)
}
