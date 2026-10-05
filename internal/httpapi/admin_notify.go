package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/jackc/pgx/v5"
)

// decisionEmail builds the email the applicant gets when a reviewer requests
// changes, approves or rejects. For changes it rotates the applicant's access
// token (only its hash is stored) so the email can carry a fresh magic link;
// call it inside the transition's transaction.
func (s *Server) decisionEmail(ctx context.Context, tx pgx.Tx, subID, to, comment string, fields []fieldComment) (*mailer.Message, error) {
	if s.Mail == nil || (to != "changes_requested" && to != "approved" && to != "rejected") {
		return nil, nil
	}
	var email, name, title string
	if err := tx.QueryRow(ctx, `
		SELECT s.applicant_email, s.applicant_name, f.title
		FROM submissions s JOIN forms f ON f.id = s.form_id WHERE s.id = $1`, subID,
	).Scan(&email, &name, &title); err != nil {
		return nil, err
	}

	hello := "Hola"
	if name = strings.TrimSpace(name); name != "" {
		hello += " " + name
	}
	var b strings.Builder
	m := &mailer.Message{To: email}
	switch to {
	case "changes_requested":
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256([]byte(token))
		if _, err := tx.Exec(ctx, `UPDATE submissions SET access_token_hash = $2 WHERE id = $1`, subID, hex.EncodeToString(sum[:])); err != nil {
			return nil, err
		}
		m.Subject = "Necesitamos algunas correcciones: " + title
		fmt.Fprintf(&b, "%s,\n\nRevisamos tu solicitud \"%s\" y necesitamos que corrijas algunos datos antes de continuar.\n\n", hello, title)
		if comment != "" {
			fmt.Fprintf(&b, "%s\n\n", comment)
		}
		if len(fields) > 0 {
			labels, _ := s.fieldLabels(ctx, tx, subID)
			b.WriteString("Campos a corregir:\n")
			for _, f := range fields {
				label := labels[f.FieldKey]
				if label == "" {
					label = f.FieldKey
				}
				fmt.Fprintf(&b, "- %s: %s\n", label, f.Body)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Corrige y reenvía desde este enlace:\n%s/s/%s\n\nNo lo compartas: cualquiera con el enlace puede ver y editar tu solicitud. Los enlaces anteriores dejan de funcionar.\n",
			strings.TrimRight(s.WebURL, "/"), token)
	case "approved":
		m.Subject = "Solicitud aprobada: " + title
		fmt.Fprintf(&b, "%s,\n\nTu solicitud \"%s\" fue aprobada. Te contactaremos con los próximos pasos.\n", hello, title)
		if comment != "" {
			fmt.Fprintf(&b, "\n%s\n", comment)
		}
	case "rejected":
		m.Subject = "Resultado de tu solicitud: " + title
		fmt.Fprintf(&b, "%s,\n\nRevisamos tu solicitud \"%s\" y no podemos aprobarla.\n\nMotivo: %s\n", hello, title, comment)
	}
	m.Text = b.String()
	return m, nil
}

// fieldLabels maps top-level field keys to their labels in the version the submission used.
func (s *Server) fieldLabels(ctx context.Context, tx pgx.Tx, subID string) (map[string]string, error) {
	var sc formSchema
	if err := tx.QueryRow(ctx, `
		SELECT v.schema FROM submissions s JOIN form_versions v ON v.id = s.form_version_id WHERE s.id = $1`, subID,
	).Scan(&sc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, sec := range sc.Sections {
		for _, f := range sec.Fields {
			out[f.Key] = labelOr(f)
		}
	}
	return out, nil
}

// sendAsync never blocks the request; failures are logged.
func (s *Server) sendAsync(m *mailer.Message) {
	if m == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Mail.Send(ctx, *m); err != nil {
			slog.Error("send decision email", "to", m.To, "err", err)
		}
	}()
}
