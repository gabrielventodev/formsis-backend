// Package forms stores forms, their editable draft and their immutable published versions.
package forms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("formulario no encontrado")
	ErrArchived  = errors.New("el formulario está archivado; restáuralo para editarlo")
	ErrNoChanges = errors.New("no hay cambios desde la última versión publicada")
	ErrPublished = errors.New("solo se pueden eliminar formularios que nunca se publicaron")
)

// InvalidSchemaError carries the problems that block publishing.
type InvalidSchemaError struct{ Problems []schema.Problem }

func (e *InvalidSchemaError) Error() string { return "el formulario tiene errores" }

type VersionInfo struct {
	ID          string    `json:"id"`
	Number      int       `json:"number"`
	PublishedAt time.Time `json:"publishedAt"`
}

type Form struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Status      string          `json:"status"`
	Schema      json.RawMessage `json:"schema,omitempty"` // the editable draft
	Current     *VersionInfo    `json:"currentVersion"`
	// True when the draft differs from the latest published version (always true before the first publish).
	HasUnpublishedChanges bool      `json:"hasUnpublishedChanges"`
	CreatedAt             time.Time `json:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

type Version struct {
	VersionInfo
	Schema json.RawMessage `json:"schema"`
}

type Store struct {
	DB *pgxpool.Pool
}

const formColumns = `
	f.id, f.title, f.description, f.status, f.draft_schema,
	v.id, v.version_number, v.published_at,
	COALESCE(f.draft_schema IS DISTINCT FROM v.schema, true),
	f.created_at, f.updated_at`

const formFrom = `
	FROM forms f
	LEFT JOIN form_versions v ON v.id = f.current_version_id`

func scanForm(row pgx.Row) (Form, error) {
	var (
		f      Form
		vID    *string
		vNum   *int
		vAt    *time.Time
		schema []byte
	)
	err := row.Scan(&f.ID, &f.Title, &f.Description, &f.Status, &schema,
		&vID, &vNum, &vAt, &f.HasUnpublishedChanges, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	f.Schema = schema
	if vID != nil {
		f.Current = &VersionInfo{ID: *vID, Number: *vNum, PublishedAt: *vAt}
	}
	return f, nil
}

// List returns the organization's forms, most recently edited first. status "" means all.
func (s *Store) List(ctx context.Context, orgID, status string) ([]Form, error) {
	rows, err := s.DB.Query(ctx, `SELECT`+formColumns+formFrom+`
		WHERE f.organization_id = $1 AND ($2 = '' OR f.status::text = $2)
		ORDER BY f.updated_at DESC`, orgID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Form{}
	for rows.Next() {
		f, err := scanForm(rows)
		if err != nil {
			return nil, err
		}
		f.Schema = nil // the list doesn't need the full draft
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, orgID, id string) (Form, error) {
	return scanForm(s.DB.QueryRow(ctx, `SELECT`+formColumns+formFrom+`
		WHERE f.organization_id = $1 AND f.id = $2`, orgID, id))
}

func (s *Store) Create(ctx context.Context, orgID, title, description string) (Form, error) {
	var id string
	err := s.DB.QueryRow(ctx, `
		INSERT INTO forms (organization_id, title, description, draft_schema)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		orgID, title, description, starterSchema).Scan(&id)
	if err != nil {
		return Form{}, err
	}
	return s.Get(ctx, orgID, id)
}

// starterSchema gives a new form one empty section so the builder has somewhere to drop fields.
const starterSchema = `{"sections":[{"key":"seccion_1","title":"Sección 1","fields":[]}]}`

type Update struct {
	Title       *string
	Description *string
	Schema      *schema.Schema
}

// UpdateDraft saves the editable draft. Published versions are never touched.
func (s *Store) UpdateDraft(ctx context.Context, orgID, id string, u Update) (Form, error) {
	var schemaJSON []byte
	if u.Schema != nil {
		b, err := json.Marshal(u.Schema)
		if err != nil {
			return Form{}, err
		}
		schemaJSON = b
	}
	var status string
	err := s.DB.QueryRow(ctx, `
		UPDATE forms SET
			title        = COALESCE($3, title),
			description  = COALESCE($4, description),
			draft_schema = COALESCE($5::jsonb, draft_schema),
			updated_at   = now()
		WHERE organization_id = $1 AND id = $2 AND status <> 'archived'
		RETURNING status`, orgID, id, u.Title, u.Description, schemaJSON).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.Get(ctx, orgID, id); gerr != nil {
			return Form{}, gerr
		}
		return Form{}, ErrArchived
	}
	if err != nil {
		return Form{}, err
	}
	return s.Get(ctx, orgID, id)
}

// Publish freezes the current draft as a new immutable version.
func (s *Store) Publish(ctx context.Context, orgID, id string) (Form, error) {
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var (
			status  string
			draft   []byte
			changed bool
		)
		err := tx.QueryRow(ctx, `
			SELECT f.status, f.draft_schema, COALESCE(f.draft_schema IS DISTINCT FROM v.schema, true)
			FROM forms f LEFT JOIN form_versions v ON v.id = f.current_version_id
			WHERE f.organization_id = $1 AND f.id = $2
			FOR UPDATE OF f`, orgID, id).Scan(&status, &draft, &changed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == "archived" {
			return ErrArchived
		}
		if !changed {
			return ErrNoChanges
		}
		sc, err := schema.Parse(draft)
		if err != nil {
			return &InvalidSchemaError{Problems: []schema.Problem{{Path: "", Message: err.Error()}}}
		}
		if problems := schema.Validate(sc); len(problems) > 0 {
			return &InvalidSchemaError{Problems: problems}
		}
		var versionID string
		err = tx.QueryRow(ctx, `
			INSERT INTO form_versions (form_id, version_number, schema)
			SELECT $1, COALESCE(MAX(version_number), 0) + 1, $2
			FROM form_versions WHERE form_id = $1
			RETURNING id`, id, draft).Scan(&versionID)
		if err != nil {
			return fmt.Errorf("insert version: %w", err)
		}
		_, err = tx.Exec(ctx, `
			UPDATE forms SET status = 'published', current_version_id = $2, updated_at = now()
			WHERE id = $1`, id, versionID)
		return err
	})
	if err != nil {
		return Form{}, err
	}
	return s.Get(ctx, orgID, id)
}

// Duplicate copies the draft into a new, unpublished form.
func (s *Store) Duplicate(ctx context.Context, orgID, id string) (Form, error) {
	var newID string
	err := s.DB.QueryRow(ctx, `
		INSERT INTO forms (organization_id, title, description, draft_schema, approval_steps)
		SELECT organization_id, title || ' (copia)', description, draft_schema, approval_steps
		FROM forms WHERE organization_id = $1 AND id = $2
		RETURNING id`, orgID, id).Scan(&newID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Form{}, ErrNotFound
	}
	if err != nil {
		return Form{}, err
	}
	return s.Get(ctx, orgID, newID)
}

// SetArchived archives a form, or restores it to published (or draft if it never was).
func (s *Store) SetArchived(ctx context.Context, orgID, id string, archived bool) (Form, error) {
	tag, err := s.DB.Exec(ctx, `
		UPDATE forms SET
			status = CASE
				WHEN $3 THEN 'archived'::form_status
				WHEN current_version_id IS NULL THEN 'draft'::form_status
				ELSE 'published'::form_status END,
			updated_at = now()
		WHERE organization_id = $1 AND id = $2`, orgID, id, archived)
	if err != nil {
		return Form{}, err
	}
	if tag.RowsAffected() == 0 {
		return Form{}, ErrNotFound
	}
	return s.Get(ctx, orgID, id)
}

// Delete removes a form that was never published (so it has no versions or submissions).
func (s *Store) Delete(ctx context.Context, orgID, id string) error {
	tag, err := s.DB.Exec(ctx, `
		DELETE FROM forms WHERE organization_id = $1 AND id = $2 AND current_version_id IS NULL`, orgID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, orgID, id); err != nil {
			return err
		}
		return ErrPublished
	}
	return nil
}

func (s *Store) Versions(ctx context.Context, orgID, id string) ([]VersionInfo, error) {
	if _, err := s.Get(ctx, orgID, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `
		SELECT id, version_number, published_at FROM form_versions
		WHERE form_id = $1 ORDER BY version_number DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionInfo{}
	for rows.Next() {
		var v VersionInfo
		if err := rows.Scan(&v.ID, &v.Number, &v.PublishedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Version(ctx context.Context, orgID, id string, number int) (Version, error) {
	var v Version
	err := s.DB.QueryRow(ctx, `
		SELECT v.id, v.version_number, v.published_at, v.schema
		FROM form_versions v JOIN forms f ON f.id = v.form_id
		WHERE f.organization_id = $1 AND f.id = $2 AND v.version_number = $3`,
		orgID, id, number).Scan(&v.ID, &v.Number, &v.PublishedAt, &v.Schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// DefaultOrganization returns the single organization the MVP runs for.
func DefaultOrganization(ctx context.Context, db *pgxpool.Pool) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT id FROM organizations ORDER BY created_at LIMIT 1`).Scan(&id)
	return id, err
}
