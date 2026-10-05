// Command seed loads a demo organization with a published KYB form and a public link, for trying the portal locally.
//
//	cd api && go run ./cmd/seed
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/gabrielventodev/formflow/api/internal/config"
	"github.com/gabrielventodev/formflow/api/internal/db"
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
        { "key": "productos", "type": "multiselect", "label": "Productos de interés", "required": true,
          "options": ["Cuenta corriente", "Pagos internacionales", "Crédito", "Factoring"] },
        { "key": "monto_mensual", "type": "number", "label": "Monto mensual estimado (USD)", "min": 0 }
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
          "accept": ["image/*", "application/pdf"], "maxMb": 5, "maxFiles": 2 },
        { "key": "acepta", "type": "checkbox", "label": "Declaro que la información entregada es verdadera.", "required": true }
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

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM form_links WHERE token = $1)`, linkToken).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		var orgID, formID, versionID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO organizations (name, slug) VALUES ('Demo', 'demo')
			ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name RETURNING id`).Scan(&orgID); err != nil {
			return err
		}
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
		_, err := tx.Exec(ctx, `INSERT INTO form_links (form_id, token, kind) VALUES ($1, $2, 'public')`, formID, linkToken)
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Formulario de prueba listo: %s/f/%s\n", cfg.WebPublicURL, linkToken)
}
