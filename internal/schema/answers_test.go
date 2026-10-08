package schema

import (
	"encoding/json"
	"testing"
)

const kyb = `{
  "sections": [
    {
      "key": "empresa",
      "title": "Datos de la empresa",
      "fields": [
        { "key": "razon_social", "type": "text", "label": "Razón social", "required": true },
        { "key": "rut", "type": "id", "idKind": "rut", "label": "RUT", "required": true },
        { "key": "tipo", "type": "select", "label": "Tipo", "options": ["SpA", "SRL", "Persona natural"] },
        { "key": "estatutos", "type": "file", "label": "Estatutos", "required": true, "accept": ["application/pdf"], "maxMb": 10,
          "showIf": { "field": "tipo", "op": "neq", "value": "Persona natural" } },
        { "key": "socios", "type": "repeater", "label": "Socios", "min": 1,
          "fields": [
            { "key": "nombre", "type": "text", "label": "Nombre", "required": true },
            { "key": "participacion", "type": "number", "label": "% participación", "min": 0, "max": 100 }
          ] }
      ]
    },
    {
      "key": "extra", "title": "Extra", "showIf": { "field": "tipo", "op": "eq", "value": "SRL" },
      "fields": [ { "key": "gerente", "type": "text", "label": "Gerente", "required": true } ]
    }
  ]
}`

func answers(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidateAnswers(t *testing.T) {
	s, err := Parse([]byte(kyb))
	if err != nil {
		t.Fatal(err)
	}
	if p := Validate(s); len(p) > 0 {
		t.Fatalf("test schema is not publishable: %v", p)
	}
	cases := []struct {
		name  string
		data  string
		files FileCounts
		want  []string
	}{
		{"empty", `{}`, nil, []string{"razon_social", "rut", "estatutos", "socios"}},
		{"ok", `{"razon_social":"Acme","rut":"76.086.428-5","tipo":"SpA","socios":[{"nombre":"Ana","participacion":50}]}`,
			FileCounts{"estatutos": 1}, nil},
		{"hidden file skipped", `{"razon_social":"Acme","rut":"76086428-5","tipo":"Persona natural","socios":[{"nombre":"Ana"}]}`,
			nil, nil},
		{"conditional section", `{"razon_social":"Acme","rut":"76086428-5","tipo":"SRL","socios":[{"nombre":"Ana"}]}`,
			FileCounts{"estatutos": 1}, []string{"gerente"}},
		{"row errors", `{"razon_social":"Acme","rut":"76.086.428-1","tipo":"SpA","socios":[{"participacion":"150"}]}`,
			FileCounts{"estatutos": 1}, []string{"rut", "socios.0.nombre", "socios.0.participacion"}},
		{"bad option", `{"razon_social":"Acme","rut":"76086428-5","tipo":"LLC","socios":[{"nombre":"Ana"}]}`,
			FileCounts{"estatutos": 1}, []string{"tipo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := s.ValidateAnswers(answers(t, c.data), c.files)
			if len(errs) != len(c.want) {
				t.Fatalf("got %v, want keys %v", errs, c.want)
			}
			for _, k := range c.want {
				if _, ok := errs[k]; !ok {
					t.Errorf("missing error for %s in %v", k, errs)
				}
			}
		})
	}
}

const kycExtra = `{
  "sections": [{
    "key": "datos", "title": "Datos",
    "fields": [
      { "key": "aviso", "type": "info", "label": "Antes de empezar", "help": "Ten a mano tu cédula" },
      { "key": "titulo", "type": "heading", "label": "Tu empresa", "help": "Datos generales", "required": true },
      { "key": "linea", "type": "divider" },
      { "key": "espacio", "type": "spacer" },
      { "key": "web", "type": "url", "label": "Sitio web" },
      { "key": "ingresos", "type": "currency", "currency": "CLP", "label": "Ingresos", "min": 0, "required": true },
      { "key": "hora", "type": "time", "label": "Hora de contacto" },
      { "key": "constitucion", "type": "datetime", "label": "Constitución" },
      { "key": "pep", "type": "yesno", "label": "¿Es PEP?", "required": true },
      { "key": "rubro", "type": "radio", "label": "Rubro", "options": ["Comercio", "Servicios"] },
      { "key": "pais", "type": "country", "label": "País", "required": true },
      { "key": "domicilio", "type": "address", "label": "Domicilio", "required": true },
      { "key": "satisfaccion", "type": "scale", "label": "Satisfacción", "min": 0, "max": 10 },
      { "key": "firma", "type": "signature", "label": "Firma", "required": true }
    ]
  }]
}`

func TestNewFieldTypes(t *testing.T) {
	s, err := Parse([]byte(kycExtra))
	if err != nil {
		t.Fatal(err)
	}
	if p := Validate(s); len(p) > 0 {
		t.Fatalf("schema is not publishable: %v", p)
	}
	const ok = `"aviso":"x","web":"https://acme.cl/x","ingresos":"1500000.5","hora":"09:30","constitucion":"2020-01-31T18:00",
		"pep":"No","rubro":"Comercio","pais":"CL","satisfaccion":10,"firma":"M10 20L30.5 40L31 41",
		"domicilio":{"line1":"Av. Siempre Viva 742","line2":"","city":"Santiago","region":"RM","country":"CL"}`
	cases := []struct {
		name string
		data string
		want []string
	}{
		{"empty", `{"domicilio":{"line1":"","city":""}}`, []string{"ingresos", "pep", "pais", "domicilio", "firma"}},
		{"ok", `{` + ok + `}`, nil},
		{"bad values", `{` + ok + `,"web":"acme.cl","ingresos":"mucho","hora":"25:00","constitucion":"2020-01-31",
			"pep":"Tal vez","rubro":"Minería","pais":"XX","satisfaccion":11,"firma":"<script>"}`,
			[]string{"web", "ingresos", "hora", "constitucion", "pep", "rubro", "pais", "satisfaccion", "firma"}},
		{"negative amount", `{` + ok + `,"ingresos":-1}`, []string{"ingresos"}},
		{"fractional scale", `{` + ok + `,"satisfaccion":2.5}`, []string{"satisfaccion"}},
		{"incomplete address", `{` + ok + `,"domicilio":{"line1":"Calle 1","country":"CL"}}`, []string{"domicilio"}},
		{"address extra key", `{` + ok + `,"domicilio":{"line1":"a","city":"b","country":"CL","x":"y"}}`, []string{"domicilio"}},
		{"address bad country", `{` + ok + `,"domicilio":{"line1":"a","city":"b","country":"ZZ"}}`, []string{"domicilio"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := s.ValidateAnswers(answers(t, c.data), nil)
			if len(errs) != len(c.want) {
				t.Fatalf("got %v, want keys %v", errs, c.want)
			}
			for _, k := range c.want {
				if _, ok := errs[k]; !ok {
					t.Errorf("missing error for %s in %v", k, errs)
				}
			}
		})
	}
	if out := s.Clean(answers(t, `{`+ok+`,"linea":"x","titulo":"y"}`)); out["aviso"] != nil || out["linea"] != nil || out["titulo"] != nil || out["domicilio"] == nil {
		t.Errorf("Clean kept info or dropped address: %v", out)
	}
}

func TestEvaluate(t *testing.T) {
	a := map[string]any{"tipo": "SpA", "productos": []any{"Crédito"}, "acepta": true, "monto": float64(10)}
	cases := []struct {
		c    Condition
		want bool
	}{
		{Condition{Field: "tipo", Op: "eq", Value: "SpA"}, true},
		{Condition{Field: "tipo", Op: "neq", Value: "SpA"}, false},
		{Condition{Field: "productos", Op: "eq", Value: "Crédito"}, true},
		{Condition{Field: "tipo", Op: "contains", Value: "sp"}, true},
		{Condition{Field: "acepta", Op: "eq", Value: true}, true},
		{Condition{Field: "nada", Op: "eq", Value: false}, true},
		{Condition{Field: "nada", Op: "empty"}, true},
		{Condition{Field: "monto", Op: "eq", Value: "10"}, true},
	}
	for _, c := range cases {
		if got := Evaluate(&c.c, a); got != c.want {
			t.Errorf("Evaluate(%+v) = %v", c.c, got)
		}
	}
}

func TestValidRUT(t *testing.T) {
	for in, want := range map[string]bool{
		"76.086.428-5": true, "11.111.111-1": true, "12345678-5": true, "10000013-K": true,
		"12345678-9": false, "abc": false, "": false,
	} {
		if got := ValidRUT(in); got != want {
			t.Errorf("ValidRUT(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCleanAndFind(t *testing.T) {
	s, _ := Parse([]byte(kyb))
	out := s.Clean(answers(t, `{"razon_social":"Acme","hack":1,"estatutos":"x","socios":[{"nombre":"Ana","x":1}]}`))
	if _, ok := out["hack"]; ok {
		t.Error("unknown key kept")
	}
	if _, ok := out["estatutos"]; ok {
		t.Error("file key kept in data")
	}
	if row := out["socios"].([]any)[0].(map[string]any); row["x"] != nil {
		t.Error("unknown row key kept")
	}
	if f := s.Find("socios.3.nombre"); f == nil || f.Label != "Nombre" {
		t.Errorf("Find repeater path = %v", f)
	}
	for _, p := range []string{"nope", "socios.x.nombre", "razon_social.0.x"} {
		if s.Find(p) != nil {
			t.Errorf("Find(%q) should be nil", p)
		}
	}
}

func TestValidPhone(t *testing.T) {
	cases := []struct {
		in, country string
		want        bool
	}{
		{"+56961234567", "", true},     // E.164, what the portal stores
		{"+5491123456789", "CL", true}, // Argentine mobile: the code wins over the field's country
		{"+56 9 6123 4567", "", true},  // typed before the country picker
		{"9 6123 4567", "", true},      // local number, read in Chile by default
		{"987654321", "PE", true},
		{"+5691234", "", false}, // too short
		{"+56961234567abc", "", false},
		{"tel: +56961234567", "", false},
		{"+56912345678", "", false}, // 912 isn't a Chilean mobile range
		{"hola", "", false},
		{"123456", "", false},
	}
	for _, c := range cases {
		if got := ValidPhone(c.in, c.country); got != c.want {
			t.Errorf("ValidPhone(%q, %q) = %v, want %v", c.in, c.country, got, c.want)
		}
	}
	if PhoneCountry("AQ") || !PhoneCountry("CL") || PhoneCountry("ZZ") {
		t.Error("PhoneCountry should accept CL and reject AQ and ZZ")
	}
}
