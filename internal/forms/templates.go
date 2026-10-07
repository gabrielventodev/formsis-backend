package forms

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Ready-made forms (KYB for companies, KYC for people) an admin can start
// from. Each template becomes an ordinary draft the admin then edits.

//go:embed templates/*.json
var templateFS embed.FS

var ErrTemplateNotFound = errors.New("plantilla no encontrada")

type Template struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Summary     string          `json:"summary"`
	Schema      json.RawMessage `json:"schema"`
}

// TemplateInfo is what the template picker lists.
type TemplateInfo struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Summary     string `json:"summary"`
	Sections    int    `json:"sections"`
	Fields      int    `json:"fields"`
}

// Templates returns every embedded template, sorted by key.
func Templates() ([]Template, error) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, err
		}
		var t Template
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("plantilla %s: %w", e.Name(), err)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func TemplateByKey(key string) (Template, error) {
	list, err := Templates()
	if err != nil {
		return Template{}, err
	}
	for _, t := range list {
		if t.Key == key {
			return t, nil
		}
	}
	return Template{}, ErrTemplateNotFound
}

// Info counts sections and input fields (display blocks excluded).
func (t Template) Info() TemplateInfo {
	var s struct {
		Sections []struct {
			Fields []struct {
				Type string `json:"type"`
			} `json:"fields"`
		} `json:"sections"`
	}
	_ = json.Unmarshal(t.Schema, &s)
	info := TemplateInfo{Key: t.Key, Title: t.Title, Description: t.Description, Summary: t.Summary, Sections: len(s.Sections)}
	for _, sec := range s.Sections {
		for _, f := range sec.Fields {
			switch f.Type {
			case "info", "heading", "divider", "spacer":
			default:
				info.Fields++
			}
		}
	}
	return info
}

// CreateFromTemplate starts a new draft form with the template's schema.
// An empty title keeps the template's own.
func (s *Store) CreateFromTemplate(ctx context.Context, orgID, key, title string) (Form, error) {
	t, err := TemplateByKey(key)
	if err != nil {
		return Form{}, err
	}
	if title == "" {
		title = t.Title
	}
	var id string
	err = s.DB.QueryRow(ctx, `
		INSERT INTO forms (organization_id, title, description, draft_schema)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		orgID, title, t.Description, string(t.Schema)).Scan(&id)
	if err != nil {
		return Form{}, err
	}
	return s.Get(ctx, orgID, id)
}
