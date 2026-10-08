package schema

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Answer validation for the filling portal. Mirrors validateAnswer() and evaluate() in
// web/src/lib/form-schema.ts so the applicant gets the same errors in the browser and on submit.

// Errors maps an answer path ("razon_social", "socios.0.nombre") to a message for the applicant.
type Errors map[string]string

// FileCounts maps a file field path to how many files are uploaded for it, and a liveness
// field key to how many completed (pass or review) liveness checks it has.
type FileCounts map[string]int

var (
	emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	rutRe   = regexp.MustCompile(`^\d{7,8}[0-9K]$`)
	dniRe   = regexp.MustCompile(`^\d{7,8}$`)
	urlRe   = regexp.MustCompile(`(?i)^https?://[^\s/$.?#][^\s]*$`)
	// SVG path data with absolute moves and lines, as drawn by the signature pad.
	signatureRe = regexp.MustCompile(`^M[\d.]+ [\d.]+(?:[ML][\d.]+ [\d.]+)*$`)
)

// MaxSignatureLen bounds a signature's path data so one answer can't bloat a submission.
const MaxSignatureLen = 60000

// Evaluate reports whether a showIf condition holds for the answers.
func Evaluate(c *Condition, answers map[string]any) bool {
	if c == nil {
		return true
	}
	v := answers[c.Field]
	if b, ok := c.Value.(bool); ok && (c.Op == "eq" || c.Op == "neq") {
		return ((v == true) == b) == (c.Op == "eq")
	}
	want := str(c.Value)
	switch c.Op {
	case "empty":
		return isEmpty(v)
	case "notEmpty":
		return !isEmpty(v)
	case "eq":
		if list, ok := v.([]any); ok {
			return containsStr(list, want)
		}
		return str(v) == want
	case "neq":
		if list, ok := v.([]any); ok {
			return !containsStr(list, want)
		}
		return str(v) != want
	case "contains":
		if list, ok := v.([]any); ok {
			return containsStr(list, want)
		}
		return strings.Contains(strings.ToLower(str(v)), strings.ToLower(want))
	}
	return true
}

// VisibleSections returns the sections whose conditions hold.
func (s Schema) VisibleSections(answers map[string]any) []Section {
	var out []Section
	for _, sec := range s.Sections {
		if Evaluate(sec.ShowIf, answers) {
			out = append(out, sec)
		}
	}
	return out
}

// ValidateAnswers checks a whole submission. Hidden sections and fields are skipped.
func (s Schema) ValidateAnswers(answers map[string]any, files FileCounts) Errors {
	errs := Errors{}
	for _, sec := range s.VisibleSections(answers) {
		validateFields(sec.Fields, answers, files, errs)
	}
	return errs
}

// ValidateSection checks one step.
func (s Schema) ValidateSection(key string, answers map[string]any, files FileCounts) Errors {
	errs := Errors{}
	for _, sec := range s.VisibleSections(answers) {
		if sec.Key == key {
			validateFields(sec.Fields, answers, files, errs)
		}
	}
	return errs
}

func validateFields(fields []Field, answers map[string]any, files FileCounts, errs Errors) {
	for _, f := range fields {
		if !Evaluate(f.ShowIf, answers) {
			continue
		}
		if msg := AnswerError(f, answers[f.Key], f.Key, files); msg != "" {
			errs[f.Key] = msg
		}
		if f.Type == TypeRepeater {
			rows, _ := answers[f.Key].([]any)
			for i, r := range rows {
				row, _ := r.(map[string]any)
				for _, sf := range f.Fields {
					p := fmt.Sprintf("%s.%d.%s", f.Key, i, sf.Key)
					if msg := AnswerError(sf, row[sf.Key], p, files); msg != "" {
						errs[p] = msg
					}
				}
			}
		}
	}
}

// AnswerError validates one answer and returns a message, or "" if it is fine.
// File fields are checked against the uploaded file count for path.
func AnswerError(f Field, v any, path string, files FileCounts) string {
	if !f.Type.HasAnswer() {
		return ""
	}
	if f.Type == TypeFile {
		if f.Required && files[path] == 0 {
			return "Adjunta un archivo"
		}
		return ""
	}
	if f.Type == TypeLiveness {
		if f.Required && files[path] == 0 {
			return "Completa la verificación con tu cámara"
		}
		return ""
	}
	if f.Type == TypeRepeater && isEmpty(v) && f.Min != nil && *f.Min > 0 {
		return fmt.Sprintf("Agrega al menos %s", num(*f.Min)) // a minimum implies the group is required
	}
	if isEmpty(v) {
		if f.Required {
			return "Este campo es obligatorio"
		}
		return ""
	}
	s := str(v)
	switch f.Type {
	case TypeText, TypeTextarea:
		n := float64(len([]rune(s)))
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("Mínimo %s caracteres", num(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("Máximo %s caracteres", num(*f.Max))
		}
	case TypeEmail:
		if !emailRe.MatchString(s) {
			return "Email no válido"
		}
	case TypePhone:
		if !ValidPhone(s, f.DefaultCountry) {
			return "Teléfono no válido"
		}
	case TypeNumber:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || math.IsNaN(n) {
			return "Debe ser un número"
		}
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("El mínimo es %s", num(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("El máximo es %s", num(*f.Max))
		}
	case TypeDate:
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return "Fecha no válida"
		}
	case TypeTime:
		if _, err := time.Parse("15:04", s); err != nil {
			return "Hora no válida"
		}
	case TypeDateTime:
		if _, err := time.Parse("2006-01-02T15:04", s); err != nil {
			return "Fecha y hora no válidas"
		}
	case TypeURL:
		if !urlRe.MatchString(s) {
			return "Dirección web no válida (debe empezar con https://)"
		}
	case TypeCurrency:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return "Debe ser un monto"
		}
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("El monto mínimo es %s", num(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("El monto máximo es %s", num(*f.Max))
		}
		return ""
	case TypeScale:
		n, err := strconv.ParseFloat(s, 64)
		lo, hi := scaleRange(f)
		if err != nil || n != math.Trunc(n) || n < lo || n > hi {
			return fmt.Sprintf("Elige un valor entre %s y %s", num(lo), num(hi))
		}
		return ""
	case TypeYesNo:
		if !slices.Contains(YesNo, s) {
			return "Opción no válida"
		}
		return ""
	case TypeCountry:
		if !ValidCountry(s) {
			return "País no válido"
		}
		return ""
	case TypeAddress:
		m, ok := v.(map[string]any)
		if !ok {
			return "Dirección no válida"
		}
		for k, x := range m {
			if _, isStr := x.(string); !slices.Contains(AddressParts, k) || !isStr {
				return "Dirección no válida"
			}
		}
		if str(m["line1"]) == "" || str(m["city"]) == "" || str(m["country"]) == "" {
			return "Completa calle, ciudad y país"
		}
		if !ValidCountry(str(m["country"])) {
			return "País no válido"
		}
		return ""
	case TypeSignature:
		if len(s) > MaxSignatureLen || !signatureRe.MatchString(s) {
			return "La firma no es válida, vuelve a firmar"
		}
		return ""
	case TypeSelect, TypeRadio:
		if !slices.Contains(f.Options, s) {
			return "Opción no válida"
		}
	case TypeMultiselect:
		list, _ := v.([]any)
		for _, x := range list {
			if !slices.Contains(f.Options, str(x)) {
				return "Opción no válida"
			}
		}
	case TypeID:
		if f.IDKind == "rut" && !ValidRUT(s) {
			return "RUT no válido"
		}
		if f.IDKind == "dni" && !dniRe.MatchString(strings.NewReplacer(".", "", " ", "").Replace(s)) {
			return "DNI no válido"
		}
	case TypeRepeater:
		n := 0
		if list, ok := v.([]any); ok {
			n = len(list)
		}
		if f.Min != nil && float64(n) < *f.Min {
			return fmt.Sprintf("Agrega al menos %s", num(*f.Min))
		}
		if f.Max != nil && float64(n) > *f.Max {
			return fmt.Sprintf("Máximo %s", num(*f.Max))
		}
		return ""
	case TypeCheckbox:
		return ""
	}
	if f.Pattern != "" {
		if re, err := regexp.Compile("^(?:" + f.Pattern + ")$"); err == nil && !re.MatchString(s) {
			if f.PatternMessage != "" {
				return f.PatternMessage
			}
			return "El formato no es válido"
		}
	}
	return ""
}

// ValidRUT checks a Chilean RUT with its check digit, e.g. "12.345.678-5".
func ValidRUT(raw string) bool {
	clean := strings.ToUpper(strings.NewReplacer(".", "", "-", "", " ", "").Replace(raw))
	if !rutRe.MatchString(clean) {
		return false
	}
	body := clean[:len(clean)-1]
	sum, mul := 0, 2
	for i := len(body) - 1; i >= 0; i-- {
		sum += int(body[i]-'0') * mul
		if mul == 7 {
			mul = 2
		} else {
			mul++
		}
	}
	var dv string
	switch rest := 11 - sum%11; rest {
	case 11:
		dv = "0"
	case 10:
		dv = "K"
	default:
		dv = strconv.Itoa(rest)
	}
	return dv == clean[len(clean)-1:]
}

// Clean keeps only answers for fields in the schema; file answers live in submission_files, not in data.
func (s Schema) Clean(answers map[string]any) map[string]any {
	out := map[string]any{}
	for _, sec := range s.Sections {
		cleanFields(sec.Fields, answers, out)
	}
	return out
}

func cleanFields(fields []Field, in, out map[string]any) {
	for _, f := range fields {
		v, ok := in[f.Key]
		if !ok || f.Type == TypeFile || f.Type == TypeLiveness || !f.Type.HasAnswer() {
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

// Find returns the field at an answer path such as "estatutos" or "socios.0.cedula".
func (s Schema) Find(path string) *Field {
	parts := strings.Split(path, ".")
	var top *Field
	for _, sec := range s.Sections {
		for i := range sec.Fields {
			if sec.Fields[i].Key == parts[0] {
				top = &sec.Fields[i]
			}
		}
	}
	switch {
	case top == nil:
		return nil
	case len(parts) == 1:
		return top
	case len(parts) == 3 && top.Type == TypeRepeater:
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return nil
		}
		for i := range top.Fields {
			if top.Fields[i].Key == parts[2] {
				return &top.Fields[i]
			}
		}
	}
	return nil
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case []any:
		return len(x) == 0
	case map[string]any:
		// An address with every part blank counts as unanswered.
		for _, p := range x {
			if !isEmpty(p) {
				return false
			}
		}
		return true
	}
	return false
}

// str matches JavaScript's String(v) for the JSON values answers can hold.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func containsStr(list []any, want string) bool {
	for _, x := range list {
		if str(x) == want {
			return true
		}
	}
	return false
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
