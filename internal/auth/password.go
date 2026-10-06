package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Password tokens let a user set their password from an emailed link: an
// invitation for a new team member, or a reset for a forgotten password.
const (
	PurposeInvite = "invite"
	PurposeReset  = "reset"

	InviteTTL = 7 * 24 * time.Hour
	ResetTTL  = time.Hour

	MinPasswordLen = 8
)

var (
	ErrInvalidToken = errors.New("el enlace no es válido o ya expiró")
	ErrWeakPassword = errors.New("la contraseña debe tener al menos 8 caracteres")
)

// CheckPassword enforces the minimum password policy.
func CheckPassword(p string) error {
	if utf8.RuneCountInString(p) < MinPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

// IssuePasswordToken creates a single-use token for the user and returns it
// raw (only its hash is stored). Earlier unused tokens of the user are revoked
// so only the latest emailed link works.
func IssuePasswordToken(ctx context.Context, tx pgx.Tx, userID, purpose string) (string, error) {
	ttl := ResetTTL
	if purpose == PurposeInvite {
		ttl = InviteTTL
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := tx.Exec(ctx, `DELETE FROM password_tokens WHERE user_id = $1 AND used_at IS NULL`, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO password_tokens (id, user_id, purpose, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), userID, purpose, time.Now().Add(ttl)); err != nil {
		return "", err
	}
	return token, nil
}

// TokenInfo describes a valid password token, for the "set your password" page.
type TokenInfo struct {
	Purpose string `json:"purpose"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	userID  string
}

func (s *Store) lookupToken(ctx context.Context, q pgx.Tx, token string, lock bool) (TokenInfo, error) {
	var ti TokenInfo
	sql := `
		SELECT t.purpose, u.email, u.name, u.id
		FROM password_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = $1 AND t.used_at IS NULL AND t.expires_at > now()
		  AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id AND m.disabled_at IS NULL)`
	if lock {
		sql += ` FOR UPDATE OF t`
	}
	var row pgx.Row
	if q != nil {
		row = q.QueryRow(ctx, sql, hashToken(strings.TrimSpace(token)))
	} else {
		row = s.DB.QueryRow(ctx, sql, hashToken(strings.TrimSpace(token)))
	}
	err := row.Scan(&ti.Purpose, &ti.Email, &ti.Name, &ti.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ti, ErrInvalidToken
	}
	return ti, err
}

// CheckToken reports who a password token belongs to without using it.
func (s *Store) CheckToken(ctx context.Context, token string) (TokenInfo, error) {
	return s.lookupToken(ctx, nil, token, false)
}

// ResetPassword sets a new password with a token and signs the user out
// everywhere. It returns the user id.
func (s *Store) ResetPassword(ctx context.Context, token, password string) (string, error) {
	if err := CheckPassword(password); err != nil {
		return "", err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	ti, err := s.lookupToken(ctx, tx, token, true)
	if err != nil {
		return "", err
	}
	if err := setPassword(ctx, tx, ti.userID, password); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE password_tokens SET used_at = now() WHERE id = $1`, hashToken(strings.TrimSpace(token))); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, ti.userID); err != nil {
		return "", err
	}
	return ti.userID, tx.Commit(ctx)
}

// ChangePassword replaces a signed-in user's password after checking the
// current one, and closes their other sessions (keepToken stays valid).
func (s *Store) ChangePassword(ctx context.Context, userID, current, password, keepToken string) error {
	if err := CheckPassword(password); err != nil {
		return err
	}
	var hash string
	if err := s.DB.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(current, hash) {
		return ErrInvalidCredentials
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := setPassword(ctx, tx, userID, password); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND id <> $2`, userID, hashToken(keepToken)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func setPassword(ctx context.Context, tx pgx.Tx, userID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash)
	return err
}

// UnusablePasswordHash is stored for invited users until they pick a password:
// it is a valid argon2id hash of a random secret nobody knows.
func UnusablePasswordHash() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return HashPassword(base64.RawStdEncoding.EncodeToString(raw))
}
