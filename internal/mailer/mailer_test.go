package mailer

import "testing"

func TestSender(t *testing.T) {
	cases := []struct{ in, header, envelope string }{
		{"Formsis <no-reply@formsis.com>", `"Formsis" <no-reply@formsis.com>`, "no-reply@formsis.com"},
		{"no-reply@formsis.com", "<no-reply@formsis.com>", "no-reply@formsis.com"},
		{"Organización <a@b.co>", "=?utf-8?q?Organizaci=C3=B3n?= <a@b.co>", "a@b.co"},
	}
	for _, c := range cases {
		h, e, err := sender(c.in)
		if err != nil {
			t.Fatalf("sender(%q): %v", c.in, err)
		}
		if h != c.header || e != c.envelope {
			t.Errorf("sender(%q) = %q, %q; want %q, %q", c.in, h, e, c.header, c.envelope)
		}
	}
	if _, _, err := sender("not an address"); err == nil {
		t.Error("sender accepted an invalid address")
	}
}
