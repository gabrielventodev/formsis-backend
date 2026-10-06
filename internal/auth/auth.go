// Package auth implements admin authentication: argon2id passwords and
// server-side sessions stored in Postgres, referenced by an httpOnly cookie.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

const (
	CookieName = "ff_session"
	SessionTTL = 7 * 24 * time.Hour
)

var ErrInvalidCredentials = errors.New("credenciales inválidas")

// Identity is the authenticated admin user, scoped to one organization.
type Identity struct {
	UserID string `json:"id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	OrgID  string `json:"organization_id"`
	Role   string `json:"role"`
}

type ctxKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity set by the session middleware.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// argon2id parameters (OWASP baseline: m=19 MiB, t=2, p=1).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns a PHC-style argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a hash produced by HashPassword.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps login timing similar whether or not the email exists.
var dummyHash, _ = HashPassword("formflow-dummy-password")

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Store reads and writes users and sessions.
type Store struct {
	DB *pgxpool.Pool
}

// Login verifies credentials and creates a session. It returns the raw cookie token.
func (s *Store) Login(ctx context.Context, email, password string) (string, Identity, error) {
	var userID, hash string
	err := s.DB.QueryRow(ctx,
		`SELECT u.id, u.password_hash FROM users u
		 WHERE lower(u.email) = lower($1)
		   AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id AND m.disabled_at IS NULL)`,
		strings.TrimSpace(email),
	).Scan(&userID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		VerifyPassword(password, dummyHash)
		return "", Identity{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", Identity{}, err
	}
	if !VerifyPassword(password, hash) {
		return "", Identity{}, ErrInvalidCredentials
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Identity{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := s.DB.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), userID, time.Now().Add(SessionTTL),
	); err != nil {
		return "", Identity{}, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID); err != nil {
		return "", Identity{}, err
	}
	id, err := s.Lookup(ctx, token)
	if err != nil {
		return "", Identity{}, err
	}
	return token, id, nil
}

// Lookup resolves a cookie token to an identity. It returns pgx.ErrNoRows when
// the session is missing, expired, or the user has no active membership.
func (s *Store) Lookup(ctx context.Context, token string) (Identity, error) {
	var id Identity
	err := s.DB.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, m.organization_id, m.role
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		JOIN memberships m ON m.user_id = u.id
		WHERE s.id = $1 AND s.expires_at > now() AND m.disabled_at IS NULL
		ORDER BY m.organization_id
		LIMIT 1`, hashToken(token),
	).Scan(&id.UserID, &id.Email, &id.Name, &id.OrgID, &id.Role)
	return id, err
}

// Logout deletes the session and opportunistically prunes expired ones.
func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM sessions WHERE id = $1 OR expires_at < now()`, hashToken(token))
	return err
}

// EnsureAdmin creates the first organization and an owner account if the
// email does not exist yet. Used to bootstrap a fresh install from env vars.
func (s *Store) EnsureAdmin(ctx context.Context, email, password, name, orgName string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)`, email).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	var orgID string
	err = tx.QueryRow(ctx, `SELECT id FROM organizations ORDER BY created_at LIMIT 1`).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`INSERT INTO organizations (name, slug) VALUES ($1, 'default') RETURNING id`, orgName,
		).Scan(&orgID)
	}
	if err != nil {
		return false, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}
	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`, email, name, hash,
	).Scan(&userID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, userID, orgID,
	); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
