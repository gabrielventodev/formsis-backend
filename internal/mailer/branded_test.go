package mailer

import (
	"context"
	"strings"
	"testing"
)

type capture struct{ got Message }

func (c *capture) Send(_ context.Context, m Message) error { c.got = m; return nil }

func TestBrandedHTML(t *testing.T) {
	inner := &capture{}
	b := Branded{Inner: inner, Brand: func(context.Context) (Brand, error) {
		return Brand{Name: "Acme <SpA>", Color: "#1d4ed8", LogoURL: "https://forms.example.com/api/v1/branding/logo?v=2", SupportEmail: "ayuda@acme.cl"}, nil
	}}
	text := "Hola Ana,\n\nRevisa tu solicitud \"<b>KYB</b>\".\n\nCorrige desde este enlace:\nhttps://forms.example.com/s/abc?x=1&y=2\n\nGracias."
	if err := b.Send(context.Background(), Message{To: "a@b.cl", Subject: "Hola", Text: text}); err != nil {
		t.Fatal(err)
	}
	h := inner.got.HTML
	for _, want := range []string{
		`href="https://forms.example.com/s/abc?x=1&amp;y=2"`, // button keeps the exact link
		"background:#1d4ed8",
		`src="https://forms.example.com/api/v1/branding/logo?v=2"`,
		"&lt;b&gt;KYB&lt;/b&gt;", // text is escaped
		"Acme &lt;SpA&gt;",
		"mailto:ayuda@acme.cl",
		"Corrige desde este enlace:",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("html missing %q:\n%s", want, h)
		}
	}
	if inner.got.Text != text {
		t.Error("plain text must be kept")
	}
}
