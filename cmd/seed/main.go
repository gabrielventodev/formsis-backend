// Command seed adds a published KYB form with a public link to the default organization, for trying the portal locally.
//
//	cd api && go run ./cmd/seed
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/gabrielventodev/formflow/api/internal/config"
	"github.com/gabrielventodev/formflow/api/internal/db"
	"github.com/gabrielventodev/formflow/api/internal/forms"
	"github.com/gabrielventodev/formflow/api/internal/schema"
	"github.com/jackc/pgx/v5"
)

const linkToken = "demo-kyb"

const demoSchema = `{
  "sections": [
    {
      "key": "empresa",
      "title": "Datos de la empresa",
      "description": "Información legal de la empresa que solicita la cuenta.",
      "fields": [
        { "key": "razon_social", "type": "text", "label": "Razón social", "required": true, "max": 200 },
        { "key": "rut_empresa", "type": "id", "idKind": "rut", "label": "RUT de la empresa", "required": true, "placeholder": "76.086.428-5" },
        { "key": "tipo", "type": "select", "label": "Tipo de sociedad", "required": true,
          "options": ["SpA", "SRL", "Sociedad Anónima", "Persona natural con giro"] },
        { "key": "fecha_constitucion", "type": "date", "label": "Fecha de constitución",
          "showIf": { "field": "tipo", "op": "neq", "value": "Persona natural con giro" } },
        { "key": "actividad", "type": "textarea", "label": "Actividad principal", "help": "Describe en pocas líneas a qué se dedica la empresa." },
        { "key": "sitio_web", "type": "url", "label": "Sitio web" },
        { "key": "domicilio", "type": "address", "label": "Domicilio comercial", "required": true },
        { "key": "productos", "type": "multiselect", "label": "Productos de interés", "required": true,
          "options": ["Cuenta corriente", "Pagos internacionales", "Crédito", "Factoring"] },
        { "key": "monto_mensual", "type": "currency", "currency": "USD", "label": "Monto mensual estimado", "min": 0 }
      ]
    },
    {
      "key": "contacto",
      "title": "Representante legal",
      "fields": [
        { "key": "rep_nombre", "type": "text", "label": "Nombre completo", "required": true },
        { "key": "rep_rut", "type": "id", "idKind": "rut", "label": "RUT", "required": true },
        { "key": "rep_email", "type": "email", "label": "Email", "required": true },
        { "key": "rep_telefono", "type": "phone", "label": "Teléfono", "placeholder": "+56 9 1234 5678" },
        { "key": "rep_nacionalidad", "type": "country", "label": "Nacionalidad", "required": true },
        { "key": "rep_pep", "type": "yesno", "label": "¿Es persona expuesta políticamente (PEP)?", "required": true,
          "help": "Ocupa o ocupó en el último año un cargo público relevante, o es familiar directo de alguien que lo hace." },
        { "key": "rep_pep_cargo", "type": "text", "label": "Cargo público", "required": true,
          "showIf": { "field": "rep_pep", "op": "eq", "value": "Sí" } },
        { "key": "socios", "type": "repeater", "label": "Socios o beneficiarios finales", "min": 1,
          "help": "Personas con 10% o más de participación.",
          "showIf": { "field": "tipo", "op": "neq", "value": "Persona natural con giro" },
          "fields": [
            { "key": "nombre", "type": "text", "label": "Nombre", "required": true },
            { "key": "rut", "type": "id", "idKind": "rut", "label": "RUT", "required": true },
            { "key": "participacion", "type": "number", "label": "% de participación", "min": 0, "max": 100, "required": true }
          ] }
      ]
    },
    {
      "key": "documentos",
      "title": "Documentos",
      "fields": [
        { "key": "estatutos", "type": "file", "label": "Estatutos o escritura de constitución", "required": true,
          "accept": ["application/pdf"], "maxMb": 10,
          "showIf": { "field": "tipo", "op": "neq", "value": "Persona natural con giro" } },
        { "key": "cedula_rep", "type": "file", "label": "Cédula del representante (ambos lados)", "required": true,
          "accept": ["image/*", "application/pdf"], "maxMb": 5 },
        { "key": "aviso_firma", "type": "info", "label": "Declaración y firma",
          "help": "Al firmar, el representante legal declara que la información y los documentos entregados son verdaderos y autoriza su verificación." },
        { "key": "acepta", "type": "checkbox", "label": "Declaro que la información entregada es verdadera.", "required": true },
        { "key": "firma", "type": "signature", "label": "Firma del representante legal", "required": true }
      ]
    }
  ]
}`

func main() {
	ctx := context.Background()
	cfg := config.Load()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}

	parsed, err := schema.Parse([]byte(demoSchema))
	if err != nil {
		log.Fatal(err)
	}
	if problems := schema.Validate(parsed); len(problems) > 0 {
		log.Fatalf("el formulario de prueba tiene errores: %v", problems)
	}

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM form_links WHERE token = $1)`, linkToken).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		orgID, err := forms.DefaultOrganization(ctx, pool)
		if err != nil {
			return err
		}
		var formID, versionID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO forms (organization_id, title, description, status, draft_schema)
			VALUES ($1, 'Onboarding empresa (KYB)', 'Completa los datos de tu empresa para abrir tu cuenta. Puedes guardar y continuar después.', 'published', $2)
			RETURNING id`, orgID, demoSchema).Scan(&formID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO form_versions (form_id, version_number, schema) VALUES ($1, 1, $2) RETURNING id`,
			formID, demoSchema).Scan(&versionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE forms SET current_version_id = $2 WHERE id = $1`, formID, versionID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO form_links (form_id, token, kind) VALUES ($1, $2, 'public')`, formID, linkToken)
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Formulario de prueba listo: %s/f/%s\n", cfg.WebPublicURL, linkToken)
}
