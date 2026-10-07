// Package schema defines the JSON shape of a form (sections, fields, validations
// and conditions) and validates it. The same shape is mirrored in web/src/lib/form-schema.ts.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

type FieldType string

const (
	TypeText        FieldType = "text"
	TypeTextarea    FieldType = "textarea"
	TypeEmail       FieldType = "email"
	TypePhone       FieldType = "phone"
	TypeNumber      FieldType = "number"
	TypeDate        FieldType = "date"
	TypeSelect      FieldType = "select"
	TypeMultiselect FieldType = "multiselect"
	TypeCheckbox    FieldType = "checkbox"
	TypeFile        FieldType = "file"
	TypeID          FieldType = "id"
	TypeRepeater    FieldType = "repeater"
	TypeURL         FieldType = "url"
	TypeCurrency    FieldType = "currency"  // amount in Field.Currency
	TypeTime        FieldType = "time"      // "15:04"
	TypeDateTime    FieldType = "datetime"  // "2006-01-02T15:04"
	TypeYesNo       FieldType = "yesno"     // "Sí" or "No"
	TypeRadio       FieldType = "radio"     // one of Options, shown as buttons
	TypeCountry     FieldType = "country"   // ISO 3166-1 alpha-2 code
	TypeAddress     FieldType = "address"   // object, see AddressParts
	TypeScale       FieldType = "scale"     // integer between Min (default 1) and Max (default 5)
	TypeSignature   FieldType = "signature" // SVG path data drawn by the applicant
	// Live camera check that a real person is filling the form. The result lives in
	// liveness_checks, not in the answers, like uploaded files live in submission_files.
	TypeLiveness FieldType = "liveness"
	// Display blocks: they lay out the form and collect no answer.
	TypeInfo    FieldType = "info"    // highlighted note with title and text
	TypeHeading FieldType = "heading" // title (Label) with optional subtitle (Help)
	TypeDivider FieldType = "divider" // horizontal line, optional caption in Label
	TypeSpacer  FieldType = "spacer"  // blank vertical space
)

var knownTypes = map[FieldType]bool{
	TypeText: true, TypeTextarea: true, TypeEmail: true, TypePhone: true, TypeNumber: true,
	TypeDate: true, TypeSelect: true, TypeMultiselect: true, TypeCheckbox: true, TypeFile: true,
	TypeID: true, TypeRepeater: true, TypeURL: true, TypeCurrency: true, TypeTime: true,
	TypeDateTime: true, TypeYesNo: true, TypeRadio: true, TypeCountry: true, TypeAddress: true,
	TypeScale: true, TypeSignature: true, TypeLiveness: true, TypeInfo: true, TypeHeading: true, TypeDivider: true,
	TypeSpacer: true,
}

// YesNo are the only answers a yesno field accepts.
var YesNo = []string{"Sí", "No"}

// AddressParts are the keys of an address answer, in display order.
var AddressParts = []string{"line1", "line2", "city", "region", "postalCode", "country"}

// IsDisplay reports whether the type is a display block (title, line, space, note) that
// collects no answer, is never required and is not shown to reviewers.
func (t FieldType) IsDisplay() bool {
	switch t {
	case TypeInfo, TypeHeading, TypeDivider, TypeSpacer:
		return true
	}
	return false
}

// HasAnswer reports whether the type collects a value from the applicant.
func (t FieldType) HasAnswer() bool { return !t.IsDisplay() }

// Conditionable reports whether other fields may depend on a field of this type.
func (t FieldType) Conditionable() bool {
	switch t {
	case TypeRepeater, TypeFile, TypeAddress, TypeSignature, TypeLiveness:
		return false
	}
	return !t.IsDisplay()
}

// ID document kinds with built-in check-digit or format validation.
var knownIDKinds = map[string]bool{"rut": true, "dni": true, "other": true}

type ConditionOp string

var knownOps = map[ConditionOp]bool{
	"eq": true, "neq": true, "contains": true, "empty": true, "notEmpty": true,
}

// Condition shows the field or section only when another field matches.
type Condition struct {
	Field string      `json:"field"`
	Op    ConditionOp `json:"op"`
	Value any         `json:"value,omitempty"`
}

type Field struct {
	Key         string    `json:"key"`
	Type        FieldType `json:"type"`
	Label       string    `json:"label"`
	Help        string    `json:"help,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
	Required    bool      `json:"required,omitempty"`
	// Number value range, text length range, or repeater item count.
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	// Message shown when Pattern does not match.
	PatternMessage string     `json:"patternMessage,omitempty"`
	Options        []string   `json:"options,omitempty"`
	Accept         []string   `json:"accept,omitempty"`
	MaxMb          *float64   `json:"maxMb,omitempty"`
	IDKind         string     `json:"idKind,omitempty"`
	Currency       string     `json:"currency,omitempty"` // ISO 4217 code for currency fields
	Fields         []Field    `json:"fields,omitempty"`   // repeater sub-fields
	ShowIf         *Condition `json:"showIf,omitempty"`
}

// Section is one step of a multi-step form.
type Section struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Fields      []Field    `json:"fields"`
	ShowIf      *Condition `json:"showIf,omitempty"`
}

type Schema struct {
	Sections []Section `json:"sections"`
}

// Parse decodes a schema, rejecting unknown properties so typos don't get stored silently.
func Parse(raw []byte) (Schema, error) {
	var s Schema
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("esquema inválido: %w", err)
	}
	if s.Sections == nil {
		s.Sections = []Section{}
	}
	return s, nil
}

// Problem is one validation error, located by a path like "sections[0].fields[2].label".
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Validate checks that a schema can be published. Drafts may be saved with problems;
// publishing requires none.
func Validate(s Schema) []Problem {
	v := validator{seen: map[string]FieldType{}, sections: map[string]bool{}}
	if len(s.Sections) == 0 {
		v.add("sections", "El formulario necesita al menos una sección")
	}
	for i, sec := range s.Sections {
		p := fmt.Sprintf("sections[%d]", i)
		if !keyRe.MatchString(sec.Key) {
			v.add(p+".key", "Clave inválida: usa minúsculas, números y guion bajo, empezando por letra")
		} else if v.sections[sec.Key] {
			v.add(p+".key", fmt.Sprintf("La clave de sección %q está repetida", sec.Key))
		}
		v.sections[sec.Key] = true
		if strings.TrimSpace(sec.Title) == "" {
			v.add(p+".title", "La sección necesita un título")
		}
		if len(sec.Fields) == 0 {
			v.add(p+".fields", "La sección necesita al menos un campo")
		}
		// A section can depend only on fields from earlier sections.
		if sec.ShowIf != nil {
			v.condition(p+".showIf", *sec.ShowIf, "")
		}
		for j, f := range sec.Fields {
			v.field(fmt.Sprintf("%s.fields[%d]", p, j), f, false)
		}
	}
	return v.problems
}

type validator struct {
	problems []Problem
	seen     map[string]FieldType // top-level field keys declared so far, in order
	sections map[string]bool
}

func (v *validator) add(path, msg string) {
	v.problems = append(v.problems, Problem{Path: path, Message: msg})
}

func (v *validator) field(p string, f Field, nested bool) {
	if !keyRe.MatchString(f.Key) {
		v.add(p+".key", "Clave inválida: usa minúsculas, números y guion bajo, empezando por letra")
	}
	if !knownTypes[f.Type] {
		v.add(p+".type", fmt.Sprintf("Tipo de campo desconocido %q", f.Type))
		return
	}
	// Lines and spaces need no text; every other block or field does.
	if strings.TrimSpace(f.Label) == "" && f.Type != TypeDivider && f.Type != TypeSpacer {
		v.add(p+".label", "El campo necesita una etiqueta")
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		v.add(p+".min", "El mínimo no puede ser mayor que el máximo")
	}
	if f.Pattern != "" {
		if _, err := regexp.Compile(f.Pattern); err != nil {
			v.add(p+".pattern", "La expresión regular no es válida")
		}
	}
	switch f.Type {
	case TypeSelect, TypeMultiselect, TypeRadio:
		if len(f.Options) == 0 {
			v.add(p+".options", "Agrega al menos una opción")
		}
		dup := map[string]bool{}
		for k, o := range f.Options {
			if strings.TrimSpace(o) == "" {
				v.add(fmt.Sprintf("%s.options[%d]", p, k), "La opción no puede estar vacía")
			} else if dup[o] {
				v.add(fmt.Sprintf("%s.options[%d]", p, k), fmt.Sprintf("La opción %q está repetida", o))
			}
			dup[o] = true
		}
	case TypeFile:
		if f.MaxMb != nil && (*f.MaxMb <= 0 || *f.MaxMb > 100) {
			v.add(p+".maxMb", "El tamaño máximo debe estar entre 0 y 100 MB")
		}
	case TypeID:
		if !knownIDKinds[f.IDKind] {
			v.add(p+".idKind", "Elige el tipo de documento (RUT, DNI u otro)")
		}
	case TypeCurrency:
		if !knownCurrencies[f.Currency] {
			v.add(p+".currency", "Elige la moneda")
		}
	case TypeScale:
		lo, hi := scaleRange(f)
		if lo != math.Trunc(lo) || hi != math.Trunc(hi) || lo < 0 || hi > 10 || lo >= hi {
			v.add(p+".min", "La escala va de un entero a otro mayor, entre 0 y 10")
		}
	case TypeLiveness:
		if nested {
			v.add(p+".type", "Un grupo repetible no puede contener una verificación de vida")
		}
	case TypeInfo, TypeHeading, TypeDivider, TypeSpacer:
		if nested {
			v.add(p+".type", "Un grupo repetible no puede contener títulos, separadores ni textos")
		}
	case TypeRepeater:
		if nested {
			v.add(p+".type", "Un grupo repetible no puede contener otro grupo repetible")
			return
		}
		if len(f.Fields) == 0 {
			v.add(p+".fields", "El grupo repetible necesita al menos un campo")
		}
		sub := map[string]bool{}
		for k, sf := range f.Fields {
			sp := fmt.Sprintf("%s.fields[%d]", p, k)
			if sub[sf.Key] {
				v.add(sp+".key", fmt.Sprintf("La clave %q está repetida dentro del grupo", sf.Key))
			}
			sub[sf.Key] = true
			if sf.ShowIf != nil {
				v.add(sp+".showIf", "Los campos dentro de un grupo repetible no admiten condiciones")
			}
			v.field(sp, sf, true)
		}
	}
	if nested {
		return
	}
	if f.ShowIf != nil {
		v.condition(p+".showIf", *f.ShowIf, f.Key)
	}
	if _, dup := v.seen[f.Key]; dup && keyRe.MatchString(f.Key) {
		v.add(p+".key", fmt.Sprintf("La clave %q está repetida", f.Key))
	}
	v.seen[f.Key] = f.Type
}

// condition checks that a showIf points at a field declared before it.
func (v *validator) condition(p string, c Condition, self string) {
	if !knownOps[c.Op] {
		v.add(p+".op", "Operador de condición desconocido")
	}
	if c.Field == self {
		v.add(p+".field", "Un campo no puede depender de sí mismo")
		return
	}
	t, ok := v.seen[c.Field]
	if !ok {
		v.add(p+".field", "La condición debe referirse a un campo anterior del formulario")
		return
	}
	if !t.Conditionable() {
		v.add(p+".field", "Ese tipo de campo no se puede usar en una condición")
	}
	if (c.Op == "eq" || c.Op == "neq" || c.Op == "contains") && c.Value == nil {
		v.add(p+".value", "Indica el valor a comparar")
	}
}

// scaleRange returns a scale field's bounds with their defaults applied.
func scaleRange(f Field) (lo, hi float64) {
	lo, hi = 1, 5
	if f.Min != nil {
		lo = *f.Min
	}
	if f.Max != nil {
		hi = *f.Max
	}
	return lo, hi
}
