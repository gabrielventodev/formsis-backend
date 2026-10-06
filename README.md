# formsis-backend

API en Go del sistema de pre-onboarding (constructor de formularios, portal de llenado y panel administrativo).

Este repositorio es el backend publicado aparte para llevarle seguimiento. Su historial viene de la carpeta `api/` del monorepo [formflow-v2](https://github.com/gabrielventodev/formflow-v2), donde vive el frontend en Next.js.

## Stack

- Go 1.26, [chi](https://github.com/go-chi/chi) para HTTP, [pgx](https://github.com/jackc/pgx) para PostgreSQL
- Migraciones con [goose](https://github.com/pressly/goose), embebidas en el binario
- Archivos en disco local o en un almacenamiento compatible con S3 (MinIO, R2)
- Correo por SMTP (opcional)

## Requisitos

- Go 1.26 o superior
- PostgreSQL 17 (también sirve 15 o 16)

## Correrlo en local

1. Levanta un Postgres. Con Docker:

   ```sh
   docker run -d --name formsis-db -p 5432:5432 \
     -e POSTGRES_USER=formflow -e POSTGRES_PASSWORD=formflow -e POSTGRES_DB=formflow \
     postgres:17-alpine
   ```

2. Arranca la API:

   ```sh
   go run ./cmd/api
   ```

   Al iniciar se aplican solas las migraciones de `migrations/`, así que no hay que correrlas a mano. Si defines `ADMIN_EMAIL` y `ADMIN_PASSWORD`, se crea esa cuenta de administrador la primera vez.

   ```sh
   ADMIN_EMAIL=admin@ejemplo.com ADMIN_PASSWORD=cambiame go run ./cmd/api
   ```

   La API queda en `http://localhost:8080`.

3. Pruebas:

   ```sh
   go test ./...
   ```

## Variables de entorno

| Variable | Por defecto | Para qué sirve |
| --- | --- | --- |
| `API_ADDR` | `:8080` | Dirección donde escucha la API |
| `DATABASE_URL` | `postgres://formflow:formflow@localhost:5432/formflow?sslmode=disable` | Conexión a Postgres |
| `WEB_ORIGIN` | `http://localhost:3000` | Origen del frontend permitido por CORS |
| `WEB_PUBLIC_URL` | igual que `WEB_ORIGIN` | URL base de los enlaces que reciben los solicitantes |
| `MAX_UPLOAD_MB` | `25` | Tamaño máximo por archivo subido |
| `STORAGE_DRIVER` | `local` | `local` (disco) o `s3` |
| `STORAGE_DIR` | `data/uploads` | Carpeta de archivos con el driver `local` |
| `S3_ENDPOINT` | `localhost:9000` | Endpoint S3 (sin `http://`) |
| `S3_BUCKET` | `formflow` | Bucket |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | vacío | Credenciales S3 |
| `S3_REGION` | `us-east-1` | Región S3 |
| `S3_USE_SSL` | `false` | `true` si el endpoint usa HTTPS |
| `SMTP_HOST` | vacío | Servidor SMTP; vacío desactiva el envío de correos |
| `SMTP_PORT` | `587` | Puerto SMTP |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | vacío | Credenciales SMTP |
| `MAIL_FROM` | `FormFlow <no-reply@localhost>` | Remitente de los correos |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | vacío | Cuenta de administrador inicial |
| `ADMIN_NAME` | `Administrador` | Nombre de esa cuenta |
| `ORG_NAME` | `Mi organización` | Nombre de la organización |
| `COOKIE_SECURE` | `false` | `true` detrás de HTTPS para marcar la cookie de sesión como Secure |

## Migraciones

Están en `migrations/` como archivos SQL de goose y se aplican al arrancar la API. Para crear una nueva, agrega el siguiente archivo numerado (por ejemplo `00007_algo.sql`) con sus secciones `-- +goose Up` y `-- +goose Down`.

## Equipo y roles

| Rol | Puede |
|---|---|
| `owner` | Todo, incluido dar o quitar el rol owner |
| `admin` | Crear formularios y enlaces, revisar, invitar y desactivar miembros (no owners), ver la actividad |
| `reviewer` | Solo revisar envíos |

- Los miembros se invitan desde `POST /api/v1/admin/team`; reciben un email con un enlace (válido 7 días) para crear su contraseña en `/admin/contrasena`. Si no hay SMTP configurado, el enlace aparece en el log de la API y también lo devuelve la respuesta (`invite_url`).
- "Olvidé mi contraseña" envía un enlace válido por 1 hora (`POST /api/v1/auth/password/forgot`).
- Desactivar a un miembro cierra sus sesiones y devuelve a la bandeja los envíos abiertos que tenía asignados.
- `GET /api/v1/admin/activity` es el historial de auditoría de toda la organización (envíos y cambios de equipo).

## Plantillas KYB / KYC

`GET /api/v1/admin/forms/templates` lista formularios listos para usar y `POST /api/v1/admin/forms/templates/{key}` crea un borrador a partir de uno (el body puede traer `{"title": "..."}`). El borrador es un formulario normal: se edita en el constructor y se publica.

- `kyb-empresa`: datos de la empresa (RUT si es chilena, ID tributario si no), representante legal con poder, beneficiarios finales (≥25 %), perfil y origen de fondos, PEP, documentos y firma.
- `kyc-persona`: identidad con documento y selfie, contacto y comprobante de domicilio, actividad e ingresos, PEP, residencia fiscal extranjera y firma.

Las plantillas viven en `internal/forms/templates/*.json` y un test comprueba que todas se pueden publicar tal cual.

## Aprobaciones en varios niveles

Cada formulario puede tener hasta 5 pasos de aprobación en orden (por ejemplo Comercial → Cumplimiento), configurados con `GET/PUT /api/v1/admin/forms/{id}/approval-flow`. Cada paso tiene nombre y, opcionalmente, una lista de aprobadores (vacía = cualquier miembro).

- "Aprobar" un envío firma el paso pendiente; el envío sigue en revisión hasta que firma el último paso. Si el paso siguiente tiene un solo aprobador, el envío se le asigna.
- Un paso con aprobadores solo lo firman ellos o un owner.
- Con dos o más pasos, una misma persona firma como máximo un paso (principio de cuatro ojos), owners incluidos.
- Pedir correcciones o reabrir una decisión reinicia el flujo; las firmas anteriores quedan en el historial como anuladas.
- Sin pasos configurados, aprobar funciona como siempre (un clic).

## Marca de la organización

`GET/PUT /api/v1/admin/organization` (owners y admins) cambia el nombre, el color principal y el email de contacto; `POST/DELETE /api/v1/admin/organization/logo` sube o quita el logo (PNG, JPG o WebP, hasta 1 MB; SVG no se acepta porque puede llevar scripts). El color debe tener contraste AA (4,5:1) con texto blanco, porque es el fondo de los botones.

El portal lo lee sin sesión desde `GET /api/v1/branding` y `GET /api/v1/branding/logo`. Todos los correos salen además en HTML con el color, el logo y el email de contacto (`mailer.Branded`); la versión de texto plano se mantiene.

## Docker

```sh
docker build -t formsis-backend .
docker run -p 8080:8080 -e DATABASE_URL=postgres://... formsis-backend
```

## Estructura

- `cmd/api`: punto de entrada
- `internal/config`: lectura de variables de entorno
- `internal/db`: conexión y migraciones
- `internal/httpapi`: rutas HTTP y middleware
- `internal/auth`: sesiones de administrador, invitaciones y cambio de contraseña
- `internal/forms` y `internal/schema`: constructor de formularios y validación del esquema
- `internal/portal`: portal de llenado (enlaces, borradores, archivos, envío)
- `internal/storage`: almacenamiento local o S3
- `internal/mailer`: envío de correos (texto + HTML con la marca)
- `internal/branding`: nombre, color y logo de la organización
- `migrations`: SQL de la base de datos
