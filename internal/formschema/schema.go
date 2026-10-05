// Package formschema describes the JSON schema of a form version and validates applicant answers against it.
// The same rules run in the browser (web/src/lib/formschema.ts) so errors show up before submitting.
package formschema

import (
	"encoding/json"
	"fmt"
)

type Schema struct {
	Sections []Section `json:"sections"`
}

type Section struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Fields      []Field    `json:"fields"`
	ShowIf      *Condition `json:"showIf,omitempty"`
}

// Field types.
const (
	TypeText        = "text"
	TypeTextarea    = "textarea"
	TypeEmail       = "email"
	TypePhone       = "phone"
	TypeNumber      = "number"
	TypeDate        = "date"
	TypeSelect      = "select"
	TypeMultiselect = "multiselect"
	TypeCheckbox    = "checkbox"
	TypeFile        = "file"
	TypeID          = "id" // national ID (RUT, DNI, etc.), see IDKind
	TypeRepeater    = "repeater"
)

type Field struct {
	Key         string     `json:"key"`
	Type        string     `json:"type"`
	Label       string     `json:"label"`
	Help        string     `json:"help,omitempty"`
	Placeholder string     `json:"placeholder,omitempty"`
	Required    bool       `json:"required,omitempty"`
	Options     []Option   `json:"options,omitempty"`
	Min         *float64   `json:"min,omitempty"` // number value, text length, repeater rows or multiselect choices
	Max         *float64   `json:"max,omitempty"`
	Pattern     string     `json:"pattern,omitempty"`
	Accept      []string   `json:"accept,omitempty"` // MIME types or extensions (".pdf") for files
	MaxMB       float64    `json:"maxMb,omitempty"`
	MaxFiles    int        `json:"maxFiles,omitempty"`
	IDKind      string     `json:"idKind,omitempty"` // "rut" validates the Chilean check digit; anything else only checks the format
	ShowIf      *Condition `json:"showIf,omitempty"`
	Fields      []Field    `json:"fields,omitempty"` // repeater row fields
}

// Option accepts either a plain string or {"value","label"} in JSON.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (o *Option) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		o.Value, o.Label = s, s
		return nil
	}
	type plain Option
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.Label == "" {
		p.Label = p.Value
	}
	*o = Option(p)
	return nil
}

// Condition shows a field or section only when another field matches.
// Ops: eq, neq, in, notIn, empty, notEmpty, gt, lt, contains.
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

func Parse(raw []byte) (*Schema, error) {
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &s, nil
}

// Find returns the field at a dotted path such as "socios.0.cedula" (row indexes are skipped).
func (s *Schema) Find(path string) *Field {
	parts := splitPath(path)
	var fields []Field
	for _, sec := range s.Sections {
		fields = append(fields, sec.Fields...)
	}
	var found *Field
	for _, p := range parts {
		if isIndex(p) {
			continue
		}
		found = nil
		for i := range fields {
			if fields[i].Key == p {
				found = &fields[i]
				break
			}
		}
		if found == nil {
			return nil
		}
		fields = found.Fields
	}
	return found
}
