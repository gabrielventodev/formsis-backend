package config

import (
	"os"
	"strconv"

	"github.com/gabrielventodev/formsis/api/internal/mailer"
	"github.com/gabrielventodev/formsis/api/internal/storage"
)

type Config struct {
	Addr        string
	DatabaseURL string
	WebOrigin   string
	// WebPublicURL is where applicants open magic links (defaults to WebOrigin).
	WebPublicURL string
	MaxUploadMB  float64
	Storage      storage.Config
	Mail         mailer.Config

	// Bootstrap owner account, created on startup if the email does not exist.
	AdminEmail    string
	AdminPassword string
	AdminName     string
	OrgName       string

	// CookieSecure marks the session cookie Secure; enable it behind HTTPS.
	CookieSecure bool

	// WebhooksAllowInsecure lets webhook endpoints use plain HTTP and private
	// or local addresses. Only for development.
	WebhooksAllowInsecure bool

	// FaceURL is the internal address of the formsis-face service (empty = liveness
	// fields are unavailable); FaceToken is the bearer token it expects.
	FaceURL   string
	FaceToken string
}

func Load() Config {
	origin := env("WEB_ORIGIN", "http://localhost:3000")
	maxMB, _ := strconv.ParseFloat(env("MAX_UPLOAD_MB", "25"), 64)
	return Config{
		Addr:         env("API_ADDR", ":8080"),
		DatabaseURL:  env("DATABASE_URL", "postgres://formsis:formsis@localhost:5432/formsis?sslmode=disable"),
		WebOrigin:    origin,
		WebPublicURL: env("WEB_PUBLIC_URL", origin),
		MaxUploadMB:  maxMB,
		Storage: storage.Config{
			Driver:    env("STORAGE_DRIVER", "local"),
			Dir:       env("STORAGE_DIR", "data/uploads"),
			Endpoint:  env("S3_ENDPOINT", "localhost:9000"),
			Bucket:    env("S3_BUCKET", "formsis"),
			AccessKey: env("S3_ACCESS_KEY", ""),
			SecretKey: env("S3_SECRET_KEY", ""),
			Region:    env("S3_REGION", "us-east-1"),
			UseSSL:    env("S3_USE_SSL", "false") == "true",
		},
		Mail: mailer.Config{
			Host:     env("SMTP_HOST", ""),
			Port:     env("SMTP_PORT", "587"),
			Username: env("SMTP_USERNAME", ""),
			Password: env("SMTP_PASSWORD", ""),
			From:     env("MAIL_FROM", "Formsis <no-reply@localhost>"),
		},
		AdminEmail:    os.Getenv("ADMIN_EMAIL"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		AdminName:     env("ADMIN_NAME", "Administrador"),
		OrgName:       env("ORG_NAME", "Mi organización"),
		CookieSecure:  os.Getenv("COOKIE_SECURE") == "true",

		WebhooksAllowInsecure: os.Getenv("WEBHOOKS_ALLOW_INSECURE") == "true",

		FaceURL:   os.Getenv("FACE_URL"),
		FaceToken: os.Getenv("FACE_TOKEN"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
