package forms

import (
	"testing"

	"github.com/gabrielventodev/formflow/api/internal/schema"
)

// Every template must be publishable as is.
func TestTemplatesAreValid(t *testing.T) {
	list, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 2 {
		t.Fatalf("want at least the KYB and KYC templates, got %d", len(list))
	}
	seen := map[string]bool{}
	for _, tpl := range list {
		if tpl.Key == "" || tpl.Title == "" || seen[tpl.Key] {
			t.Errorf("template %q: missing or duplicate key/title", tpl.Key)
		}
		seen[tpl.Key] = true
		s, err := schema.Parse(tpl.Schema)
		if err != nil {
			t.Errorf("%s: parse: %v", tpl.Key, err)
			continue
		}
		for _, p := range schema.Validate(s) {
			t.Errorf("%s: %+v", tpl.Key, p)
		}
		if info := tpl.Info(); info.Fields < 10 {
			t.Errorf("%s: only %d fields", tpl.Key, info.Fields)
		}
	}
}
