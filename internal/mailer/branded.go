package mailer

import (
	"bytes"
	"context"
	"html"
	"html/template"
	"log/slog"
	"regexp"
	"strings"
)

// Brand is how an organization's emails look.
type Brand struct {
	Name         string
	Color        string // #rrggbb, readable behind white text
	LogoURL      string // absolute URL, or ""
	SupportEmail string
}

// Branded adds an HTML version in the organization's colors to every
// plain-text email. If the brand cannot be loaded the email goes out as text.
type Branded struct {
	Inner Mailer
	Brand func(ctx context.Context) (Brand, error)
}

func (b Branded) Send(ctx context.Context, m Message) error {
	if m.HTML == "" && b.Brand != nil {
		if br, err := b.Brand(ctx); err != nil {
			slog.Warn("email branding unavailable", "err", err)
		} else {
			m.HTML = RenderHTML(br, m)
		}
	}
	return b.Inner.Send(ctx, m)
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"]+`)

type block struct {
	Button template.URL
	Body   template.HTML
}

// RenderHTML turns a plain-text email into simple branded HTML: paragraphs
// stay paragraphs, URLs become links, and a paragraph that is only a link
// (after an optional lead-in line) becomes a button.
func RenderHTML(br Brand, m Message) string {
	var blocks []block
	for _, para := range strings.Split(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		lines := strings.Split(para, "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if urlRe.FindString(last) == last {
			if len(lines) > 1 {
				blocks = append(blocks, block{Body: linkify(strings.Join(lines[:len(lines)-1], "\n"))})
			}
			blocks = append(blocks, block{Button: template.URL(last)})
			continue
		}
		blocks = append(blocks, block{Body: linkify(para)})
	}
	color := br.Color
	if color == "" {
		color = "#18181b"
	}
	var out bytes.Buffer
	_ = emailTmpl.Execute(&out, map[string]any{
		"Brand": br, "Color": template.CSS(color), "Subject": m.Subject, "Blocks": blocks,
	})
	return out.String()
}

func linkify(s string) template.HTML {
	var b strings.Builder
	rest := s
	for {
		loc := urlRe.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(html.EscapeString(rest))
			break
		}
		b.WriteString(html.EscapeString(rest[:loc[0]]))
		u := rest[loc[0]:loc[1]]
		b.WriteString(`<a href="` + html.EscapeString(u) + `" style="color:inherit">` + html.EscapeString(u) + `</a>`)
		rest = rest[loc[1]:]
	}
	return template.HTML(strings.ReplaceAll(b.String(), "\n", "<br>\n"))
}

var emailTmpl = template.Must(template.New("email").Parse(`<!doctype html>
<html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>{{.Subject}}</title></head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#18181b">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:24px 12px">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border-radius:12px;overflow:hidden;border:1px solid #e4e4e7">
<tr><td style="background:{{.Color}};height:6px;font-size:0;line-height:0">&nbsp;</td></tr>
<tr><td style="padding:24px 32px 8px">
{{if .Brand.LogoURL}}<img src="{{.Brand.LogoURL}}" alt="{{.Brand.Name}}" height="40" style="display:block;height:40px;max-width:200px;border:0">{{else}}<span style="font-size:18px;font-weight:700">{{.Brand.Name}}</span>{{end}}
</td></tr>
<tr><td style="padding:8px 32px 24px;font-size:15px;line-height:1.6">
{{range .Blocks}}{{if .Button}}<p style="margin:20px 0"><a href="{{.Button}}" style="display:inline-block;background:{{$.Color}};color:#ffffff;text-decoration:none;font-weight:600;padding:12px 20px;border-radius:8px">Abrir enlace</a></p>
<p style="margin:0 0 16px;font-size:12px;color:#71717a;word-break:break-all">{{.Button}}</p>
{{else}}<p style="margin:0 0 16px">{{.Body}}</p>
{{end}}{{end}}
</td></tr>
<tr><td style="padding:16px 32px;border-top:1px solid #e4e4e7;font-size:12px;color:#71717a">{{.Brand.Name}}{{if .Brand.SupportEmail}} · ¿Dudas? Escríbenos a <a href="mailto:{{.Brand.SupportEmail}}" style="color:#71717a">{{.Brand.SupportEmail}}</a>{{end}}</td></tr>
</table></td></tr></table></body></html>
`))
