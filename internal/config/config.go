package config

import "os"

type Config struct {
	Addr        string
	DatabaseURL string
	WebOrigin   string
}

func Load() Config {
	return Config{
		Addr:        env("API_ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", "postgres://formflow:formflow@localhost:5432/formflow?sslmode=disable"),
		WebOrigin:   env("WEB_ORIGIN", "http://localhost:3000"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
