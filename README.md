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

Están en `migrations/` como archivos SQL de goose y se aplican al arrancar la API. Para crear una nueva, agrega el siguiente archivo numerado (por ejemplo `00005_algo.sql`) con sus secciones `-- +goose Up` y `-- +goose Down`.

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
- `internal/auth`: sesiones de administrador
- `internal/forms` y `internal/schema`: constructor de formularios y validación del esquema
- `internal/portal`: portal de llenado (enlaces, borradores, archivos, envío)
- `internal/storage`: almacenamiento local o S3
- `internal/mailer`: envío de correos
- `migrations`: SQL de la base de datos
