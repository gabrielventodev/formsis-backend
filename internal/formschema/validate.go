package formschema

import (
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Errors maps a field path ("razon_social", "socios.0.nombre") to a message for the applicant.
type Errors map[string]string

// FileCounts maps a file field path to how many files are uploaded for it.
type FileCounts map[string]int

var (
	phoneRe = regexp.MustCompile(`^\+?[0-9 ()\-]{6,20}$`)
	idRe    = regexp.MustCompile(`^[0-9A-Za-z.\-]{4,20}$`)
)

// Validate checks the answers of a whole submission. Hidden fields and sections are skipped.
func (s *Schema) Validate(data map[string]any, files FileCounts) Errors {
	errs := Errors{}
	for _, sec := range s.Sections {
		if !visible(sec.ShowIf, data, data) {
			continue
		}
		validateFields(sec.Fields, data, data, "", files, errs)
	}
	return errs
}

// ValidateSection checks one step, so the portal can block "Siguiente" on errors.
func (s *Schema) ValidateSection(key string, data map[string]any, files FileCounts) Errors {
	errs := Errors{}
	for _, sec := range s.Sections {
		if sec.Key == key && visible(sec.ShowIf, data, data) {
			validateFields(sec.Fields, data, data, "", files, errs)
		}
	}
	return errs
}

func validateFields(fields []Field, scope, root map[string]any, prefix string, files FileCounts, errs Errors) {
	for _, f := range fields {
		if !visible(f.ShowIf, scope, root) {
			continue
		}
		path := prefix + f.Key
		if msg := validateField(f, scope[f.Key], root, path, files, errs); msg != "" {
			errs[path] = msg
		}
	}
}

func validateField(f Field, v any, root map[string]any, path string, files FileCounts, errs Errors) string {
	switch f.Type {
	case TypeFile:
		n := files[path]
		if f.Required && n == 0 {
			return "Adjunta un archivo."
		}
		if f.MaxFiles > 0 && n > f.MaxFiles {
			return fmt.Sprintf("Máximo %d archivos.", f.MaxFiles)
		}
		return ""
	case TypeRepeater:
		rows, _ := v.([]any)
		if f.Required && len(rows) == 0 {
			return "Agrega al menos un elemento."
		}
		if f.Min != nil && float64(len(rows)) < *f.Min {
			return fmt.Sprintf("Agrega al menos %s.", fmtNum(*f.Min))
		}
		if f.Max != nil && float64(len(rows)) > *f.Max {
			return fmt.Sprintf("Máximo %s elementos.", fmtNum(*f.Max))
		}
		for i, r := range rows {
			row, _ := r.(map[string]any)
			if row == nil {
				row = map[string]any{}
			}
			validateFields(f.Fields, row, root, fmt.Sprintf("%s.%d.", path, i), files, errs)
		}
		return ""
	case TypeCheckbox:
		b, _ := v.(bool)
		if f.Required && !b {
			return "Debes marcar esta casilla."
		}
		return ""
	case TypeMultiselect:
		list := toStrings(v)
		if f.Required && len(list) == 0 {
			return "Elige al menos una opción."
		}
		for _, x := range list {
			if !hasOption(f.Options, x) {
				return "Opción no válida."
			}
		}
		if f.Min != nil && float64(len(list)) < *f.Min && len(list) > 0 {
			return fmt.Sprintf("Elige al menos %s.", fmtNum(*f.Min))
		}
		if f.Max != nil && float64(len(list)) > *f.Max {
			return fmt.Sprintf("Elige como máximo %s.", fmtNum(*f.Max))
		}
		return ""
	}

	if isEmpty(v) {
		if f.Required {
			return "Este campo es obligatorio."
		}
		return ""
	}

	switch f.Type {
	case TypeNumber:
		n, ok := toNumber(v)
		if !ok {
			return "Ingresa un número."
		}
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("Debe ser mayor o igual a %s.", fmtNum(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("Debe ser menor o igual a %s.", fmtNum(*f.Max))
		}
		return ""
	case TypeSelect:
		s, _ := v.(string)
		if !hasOption(f.Options, s) {
			return "Opción no válida."
		}
		return ""
	}

	s, ok := v.(string)
	if !ok {
		return "Valor no válido."
	}
	s = strings.TrimSpace(s)
	switch f.Type {
	case TypeEmail:
		if !ValidEmail(s) {
			return "Ingresa un email válido."
		}
	case TypePhone:
		if !phoneRe.MatchString(s) {
			return "Ingresa un teléfono válido."
		}
	case TypeDate:
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return "Ingresa una fecha válida."
		}
	case TypeID:
		if strings.EqualFold(f.IDKind, "rut") {
			if !ValidRUT(s) {
				return "RUT no válido."
			}
		} else if !idRe.MatchString(s) {
			return "Número de identificación no válido."
		}
	}
	if f.Type == TypeText || f.Type == TypeTextarea {
		n := float64(len([]rune(s)))
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("Mínimo %s caracteres.", fmtNum(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("Máximo %s caracteres.", fmtNum(*f.Max))
		}
	}
	if f.Pattern != "" {
		re, err := regexp.Compile("^(?:" + f.Pattern + ")$")
		if err == nil && !re.MatchString(s) {
			return "El formato no es válido."
		}
	}
	return ""
}

// ValidEmail accepts a bare address such as "ana@empresa.cl".
func ValidEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && strings.Contains(s[strings.LastIndex(s, "@")+1:], ".")
}

// ValidRUT checks a Chilean RUT such as "12.345.678-5".
func ValidRUT(s string) bool {
	s = strings.ToUpper(strings.NewReplacer(".", "", "-", "", " ", "").Replace(s))
	if len(s) < 2 {
		return false
	}
	body, dv := s[:len(s)-1], s[len(s)-1]
	n, err := strconv.Atoi(body)
	if err != nil || n <= 0 {
		return false
	}
	sum, mul := 0, 2
	for ; n > 0; n /= 10 {
		sum += (n % 10) * mul
		mul++
		if mul > 7 {
			mul = 2
		}
	}
	want := 11 - sum%11
	switch want {
	case 11:
		return dv == '0'
	case 10:
		return dv == 'K'
	default:
		return dv == byte('0'+want)
	}
}

// visible evaluates showIf, looking the referenced key up in the row first and then at the top level.
func visible(c *Condition, scope, root map[string]any) bool {
	if c == nil || c.Field == "" {
		return true
	}
	v, ok := scope[c.Field]
	if !ok {
		v = root[c.Field]
	}
	switch c.Op {
	case "", "eq":
		return equal(v, c.Value)
	case "neq":
		return !equal(v, c.Value)
	case "in":
		for _, x := range toAnySlice(c.Value) {
			if equal(v, x) {
				return true
			}
		}
		return false
	case "notIn":
		for _, x := range toAnySlice(c.Value) {
			if equal(v, x) {
				return false
			}
		}
		return true
	case "empty":
		return isEmpty(v)
	case "notEmpty":
		return !isEmpty(v)
	case "gt", "lt":
		a, ok1 := toNumber(v)
		b, ok2 := toNumber(c.Value)
		if !ok1 || !ok2 {
			return false
		}
		if c.Op == "gt" {
			return a > b
		}
		return a < b
	case "contains":
		for _, x := range toStrings(v) {
			if equal(x, c.Value) {
				return true
			}
		}
		return false
	}
	return true
}

// Clean keeps only answers for fields that exist in the schema, so stray keys never reach the database.
func (s *Schema) Clean(data map[string]any) map[string]any {
	out := map[string]any{}
	for _, sec := range s.Sections {
		cleanFields(sec.Fields, data, out)
	}
	return out
}

func cleanFields(fields []Field, in, out map[string]any) {
	for _, f := range fields {
		v, ok := in[f.Key]
		if !ok || f.Type == TypeFile {
			continue
		}
		if f.Type == TypeRepeater {
			rows, _ := v.([]any)
			clean := make([]any, 0, len(rows))
			for _, r := range rows {
				row, _ := r.(map[string]any)
				nr := map[string]any{}
				if row != nil {
					cleanFields(f.Fields, row, nr)
				}
				clean = append(clean, nr)
			}
			out[f.Key] = clean
			continue
		}
		out[f.Key] = v
	}
}

func equal(a, b any) bool {
	if na, ok := toNumber(a); ok {
		if nb, ok := toNumber(b); ok {
			return na == nb
		}
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case bool:
		return !x
	}
	return false
}

func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x)
	case int:
		return float64(x), true
	case string:
		n, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(x), ",", "."), 64)
		return n, err == nil
	}
	return 0, false
}

func toStrings(v any) []string {
	var out []string
	for _, x := range toAnySlice(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toAnySlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return nil
}

func hasOption(opts []Option, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func splitPath(p string) []string { return strings.Split(p, ".") }

func isIndex(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}
