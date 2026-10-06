package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Team management: owners and admins invite members, change their role and
// deactivate them. Rules:
//   - only an owner can grant the owner role or change another owner;
//   - nobody changes their own role or deactivates themselves;
//   - the organization always keeps at least one active owner.
func (s *Server) teamRoutes(r chi.Router) {
	r.Get("/", s.listTeam)
	r.Group(func(r chi.Router) {
		r.Use(requireRole("owner", "admin"))
		r.Post("/", s.inviteMember)
		r.Patch("/{userID}", s.updateMember)
		r.Post("/{userID}/invite", s.resendInvite)
	})
}

type memberRow struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	Active      bool       `json:"active"`
	Pending     bool       `json:"pending"` // invited, has not set a password yet
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

const memberSelect = `
	SELECT u.id, u.email, u.name, m.role, m.disabled_at IS NULL,
	       u.last_login_at IS NULL AND EXISTS (
	           SELECT 1 FROM password_tokens t
	           WHERE t.user_id = u.id AND t.purpose = 'invite' AND t.used_at IS NULL),
	       m.created_at, u.last_login_at
	FROM memberships m JOIN users u ON u.id = m.user_id`

func scanMember(row pgx.Row) (memberRow, error) {
	var m memberRow
	err := row.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.Active, &m.Pending, &m.CreatedAt, &m.LastLoginAt)
	return m, err
}

func (s *Server) listTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	rows, err := s.DB.Query(r.Context(), memberSelect+`
		WHERE m.organization_id = $1
		ORDER BY m.disabled_at IS NOT NULL, lower(COALESCE(NULLIF(u.name, ''), u.email))`, id.OrgID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	out := []memberRow{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func validRole(role string) bool {
	return role == "owner" || role == "admin" || role == "reviewer"
}

func (s *Server) inviteMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Name = strings.TrimSpace(in.Name)
	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email {
		writeError(w, http.StatusBadRequest, "Ingresa un email válido", nil)
		return
	}
	if len(in.Name) > 200 {
		writeError(w, http.StatusBadRequest, "El nombre es demasiado largo", nil)
		return
	}
	if !validRole(in.Role) {
		writeError(w, http.StatusBadRequest, "Elige un rol: owner, admin o reviewer", nil)
		return
	}
	if in.Role == "owner" && id.Role != "owner" {
		writeError(w, http.StatusForbidden, "Solo un owner puede invitar a otro owner", nil)
		return
	}

	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)`, in.Email).Scan(&exists); err != nil {
		s.serverError(w, r, err)
		return
	}
	if exists {
		writeError(w, http.StatusConflict, "Ya existe un usuario con ese email", nil)
		return
	}
	hash, err := auth.UnusablePasswordHash()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`, in.Email, in.Name, hash,
	).Scan(&userID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, $3)`, userID, id.OrgID, in.Role,
	); err != nil {
		s.serverError(w, r, err)
		return
	}
	token, err := auth.IssuePasswordToken(ctx, tx, userID, auth.PurposeInvite)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := insertOrgAudit(ctx, tx, id.OrgID, id.UserID, "member.invited",
		map[string]any{"user_id": userID, "email": in.Email, "role": in.Role}); err != nil {
		s.serverError(w, r, err)
		return
	}
	m, err := scanMember(tx.QueryRow(ctx, memberSelect+` WHERE m.user_id = $1 AND m.organization_id = $2`, userID, id.OrgID))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	link := s.passwordLink(token)
	s.sendMail(s.inviteEmail(id, in.Email, in.Name, in.Role, link))
	writeJSON(w, http.StatusCreated, map[string]any{"member": m, "invite_url": link})
}

func (s *Server) resendInvite(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	m, err := scanMember(tx.QueryRow(ctx, memberSelect+` WHERE m.user_id::text = $1 AND m.organization_id = $2`,
		chi.URLParam(r, "userID"), id.OrgID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Miembro no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !m.Active {
		writeError(w, http.StatusConflict, "Reactiva al miembro antes de enviarle un enlace", nil)
		return
	}
	if m.Role == "owner" && id.Role != "owner" {
		writeError(w, http.StatusForbidden, "Solo un owner puede gestionar a otro owner", nil)
		return
	}
	// Someone who already signed in gets a reset link; a pending invitee a new invitation.
	purpose := auth.PurposeInvite
	if m.LastLoginAt != nil {
		purpose = auth.PurposeReset
	}
	token, err := auth.IssuePasswordToken(ctx, tx, m.ID, purpose)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := insertOrgAudit(ctx, tx, id.OrgID, id.UserID, "member.link_sent",
		map[string]any{"user_id": m.ID, "email": m.Email, "purpose": purpose}); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	link := s.passwordLink(token)
	if purpose == auth.PurposeInvite {
		s.sendMail(s.inviteEmail(id, m.Email, m.Name, m.Role, link))
	} else {
		s.sendMail(s.resetEmail(m.Email, m.Name, link))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invite_url": link, "purpose": purpose})
}

func (s *Server) updateMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		Role   *string `json:"role"`
		Active *bool   `json:"active"`
		Name   *string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	if in.Role != nil && !validRole(*in.Role) {
		writeError(w, http.StatusBadRequest, "Elige un rol: owner, admin o reviewer", nil)
		return
	}
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		if len(*in.Name) > 200 {
			writeError(w, http.StatusBadRequest, "El nombre es demasiado largo", nil)
			return
		}
	}

	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	// Lock every owner row so two concurrent demotions cannot leave the org without one.
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM memberships WHERE organization_id = $1 AND role = 'owner' FOR UPDATE`, id.OrgID); err != nil {
		s.serverError(w, r, err)
		return
	}
	cur, err := scanMember(tx.QueryRow(ctx, memberSelect+` WHERE m.user_id::text = $1 AND m.organization_id = $2 FOR UPDATE OF m`,
		chi.URLParam(r, "userID"), id.OrgID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Miembro no encontrado", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	roleChange := in.Role != nil && *in.Role != cur.Role
	activeChange := in.Active != nil && *in.Active != cur.Active
	if cur.ID == id.UserID && (roleChange || activeChange) {
		writeError(w, http.StatusConflict, "No puedes cambiar tu propio rol ni desactivarte", nil)
		return
	}
	if (cur.Role == "owner" || (in.Role != nil && *in.Role == "owner")) && id.Role != "owner" && (roleChange || activeChange || in.Name != nil) {
		writeError(w, http.StatusForbidden, "Solo un owner puede gestionar el rol owner", nil)
		return
	}
	if cur.Role == "owner" && cur.Active && ((roleChange) || (activeChange && !*in.Active)) {
		var owners int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM memberships
			WHERE organization_id = $1 AND role = 'owner' AND disabled_at IS NULL`, id.OrgID).Scan(&owners); err != nil {
			s.serverError(w, r, err)
			return
		}
		if owners <= 1 {
			writeError(w, http.StatusConflict, "La organización necesita al menos un owner activo", nil)
			return
		}
	}

	meta := map[string]any{"user_id": cur.ID, "email": cur.Email}
	if roleChange {
		if _, err := tx.Exec(ctx, `UPDATE memberships SET role = $3 WHERE user_id = $1 AND organization_id = $2`,
			cur.ID, id.OrgID, *in.Role); err != nil {
			s.serverError(w, r, err)
			return
		}
		meta["from_role"], meta["to_role"] = cur.Role, *in.Role
	}
	if activeChange {
		if _, err := tx.Exec(ctx, `
			UPDATE memberships SET disabled_at = CASE WHEN $3 THEN NULL ELSE now() END
			WHERE user_id = $1 AND organization_id = $2`, cur.ID, id.OrgID, *in.Active); err != nil {
			s.serverError(w, r, err)
			return
		}
		if !*in.Active {
			// Sign them out now and release their open cases back to the queue.
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, cur.ID); err != nil {
				s.serverError(w, r, err)
				return
			}
			if _, err := tx.Exec(ctx, `DELETE FROM password_tokens WHERE user_id = $1 AND used_at IS NULL`, cur.ID); err != nil {
				s.serverError(w, r, err)
				return
			}
			tag, err := tx.Exec(ctx, `
				UPDATE submissions SET assigned_to = NULL, updated_at = now()
				WHERE organization_id = $1 AND assigned_to = $2
				  AND status IN ('submitted', 'in_review', 'changes_requested')`, id.OrgID, cur.ID)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			meta["unassigned"] = tag.RowsAffected()
		}
		meta["active"] = *in.Active
	}
	if in.Name != nil && *in.Name != cur.Name {
		if _, err := tx.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, cur.ID, *in.Name); err != nil {
			s.serverError(w, r, err)
			return
		}
		meta["name"] = *in.Name
	}

	if len(meta) > 2 {
		action := "member.updated"
		switch {
		case activeChange && !*in.Active:
			action = "member.deactivated"
		case activeChange:
			action = "member.reactivated"
		case roleChange:
			action = "member.role_changed"
		}
		if err := insertOrgAudit(ctx, tx, id.OrgID, id.UserID, action, meta); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	m, err := scanMember(tx.QueryRow(ctx, memberSelect+` WHERE m.user_id = $1 AND m.organization_id = $2`, cur.ID, id.OrgID))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// insertOrgAudit records an organization-level event (no submission attached).
func insertOrgAudit(ctx context.Context, tx pgx.Tx, orgID, userID, action string, meta map[string]any) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (organization_id, actor_type, actor_id, action, metadata)
		VALUES ($1, 'user', $2, $3, $4)`, orgID, userID, action, b)
	return err
}

// ---- Password links ----

func (s *Server) passwordLink(token string) string {
	return strings.TrimRight(s.WebURL, "/") + "/admin/contrasena?token=" + token
}

var roleLabels = map[string]string{"owner": "Owner", "admin": "Administrador", "reviewer": "Revisor"}

func (s *Server) inviteEmail(by auth.Identity, email, name, role, link string) *mailer.Message {
	hello := "Hola"
	if name != "" {
		hello += " " + name
	}
	inviter := by.Name
	if inviter == "" {
		inviter = by.Email
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s,\n\n%s te invitó al panel de revisión de solicitudes con el rol %s.\n\n", hello, inviter, roleLabels[role])
	fmt.Fprintf(&b, "Crea tu contraseña desde este enlace (vence en 7 días):\n%s\n\nTu usuario es %s.\n", link, email)
	return &mailer.Message{To: email, Subject: "Te invitaron al panel de solicitudes", Text: b.String()}
}

func (s *Server) resetEmail(email, name, link string) *mailer.Message {
	hello := "Hola"
	if name != "" {
		hello += " " + name
	}
	text := fmt.Sprintf("%s,\n\nRecibimos una solicitud para cambiar la contraseña de %s.\n\nCrea una nueva desde este enlace (vence en 1 hora):\n%s\n\nSi no fuiste tú, ignora este mensaje: tu contraseña actual sigue funcionando.\n",
		hello, email, link)
	return &mailer.Message{To: email, Subject: "Cambia tu contraseña", Text: text}
}

func (s *Server) sendMail(m *mailer.Message) {
	if s.Mail == nil || m == nil {
		if m != nil {
			slog.Info("email not sent (no mailer)", "to", m.To, "subject", m.Subject)
		}
		return
	}
	s.sendAsync(m)
}

// forgotPassword always answers 204 so it cannot be used to discover accounts.
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || strings.TrimSpace(in.Email) == "" {
		writeError(w, http.StatusBadRequest, "Ingresa tu email", nil)
		return
	}
	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	var userID, email, name string
	var recent bool
	err = tx.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, EXISTS (
			SELECT 1 FROM password_tokens t
			WHERE t.user_id = u.id AND t.purpose = 'reset' AND t.created_at > now() - interval '1 minute')
		FROM users u
		WHERE lower(u.email) = lower($1)
		  AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id AND m.disabled_at IS NULL)`,
		strings.TrimSpace(in.Email)).Scan(&userID, &email, &name, &recent)
	if errors.Is(err, pgx.ErrNoRows) || recent {
		w.WriteHeader(http.StatusNoContent) // unknown, disabled, or a link was just sent
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	token, err := auth.IssuePasswordToken(ctx, tx, userID, auth.PurposeReset)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.sendMail(s.resetEmail(email, name, s.passwordLink(token)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) checkPasswordToken(w http.ResponseWriter, r *http.Request) {
	ti, err := s.authStore().CheckToken(r.Context(), r.URL.Query().Get("token"))
	if errors.Is(err, auth.ErrInvalidToken) {
		writeError(w, http.StatusNotFound, "El enlace no es válido o ya expiró. Pide uno nuevo.", nil)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ti)
}

// resetPassword sets the password from an emailed link and signs the user in.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	ctx := r.Context()
	store := s.authStore()
	ti, err := store.CheckToken(ctx, in.Token)
	if err == nil {
		_, err = store.ResetPassword(ctx, in.Token, in.Password)
	}
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		writeError(w, http.StatusNotFound, "El enlace no es válido o ya expiró. Pide uno nuevo.", nil)
		return
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "La contraseña debe tener al menos 8 caracteres", nil)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	token, id, err := store.Login(ctx, ti.Email, in.Password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.SetCookie(w, s.sessionCookie(token, time.Now().Add(auth.SessionTTL)))
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var in struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Solicitud inválida", nil)
		return
	}
	c, _ := r.Cookie(auth.CookieName)
	err := s.authStore().ChangePassword(r.Context(), id.UserID, in.Current, in.Password, c.Value)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusBadRequest, "La contraseña actual no es correcta", nil)
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "La contraseña nueva debe tener al menos 8 caracteres", nil)
	case err != nil:
		s.serverError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
