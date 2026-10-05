package config

import (
	"os"
	"strconv"

	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/gabrielventodev/formflow/api/internal/storage"
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
}

func Load() Config {
	origin := env("WEB_ORIGIN", "http://localhost:3000")
	maxMB, _ := strconv.ParseFloat(env("MAX_UPLOAD_MB", "25"), 64)
	return Config{
		Addr:         env("API_ADDR", ":8080"),
		DatabaseURL:  env("DATABASE_URL", "postgres://formflow:formflow@localhost:5432/formflow?sslmode=disable"),
		WebOrigin:    origin,
		WebPublicURL: env("WEB_PUBLIC_URL", origin),
		MaxUploadMB:  maxMB,
		Storage: storage.Config{
			Driver:    env("STORAGE_DRIVER", "local"),
			Dir:       env("STORAGE_DIR", "data/uploads"),
			Endpoint:  env("S3_ENDPOINT", "localhost:9000"),
			Bucket:    env("S3_BUCKET", "formflow"),
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
			From:     env("MAIL_FROM", "FormFlow <no-reply@localhost>"),
		},
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
