package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gabrielventodev/formflow/api/internal/auth"
	"github.com/gabrielventodev/formflow/api/internal/branding"
	"github.com/gabrielventodev/formflow/api/internal/config"
	"github.com/gabrielventodev/formflow/api/internal/db"
	"github.com/gabrielventodev/formflow/api/internal/face"
	"github.com/gabrielventodev/formflow/api/internal/forms"
	"github.com/gabrielventodev/formflow/api/internal/httpapi"
	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/gabrielventodev/formflow/api/internal/portal"
	"github.com/gabrielventodev/formflow/api/internal/storage"
	"github.com/gabrielventodev/formflow/api/internal/webhooks"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	slog.Info("migrations applied")

	if cfg.AdminEmail != "" && cfg.AdminPassword != "" {
		created, err := (&auth.Store{DB: pool}).EnsureAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword, cfg.AdminName, cfg.OrgName)
		if err != nil {
			return err
		}
		if created {
			slog.Info("admin account created", "email", cfg.AdminEmail)
		}
	}

	orgID, err := forms.DefaultOrganization(ctx, pool)
	if err != nil {
		return fmt.Errorf("load organization: %w", err)
	}
	store, err := storage.New(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	// Every email gets an HTML version in the organization's colors and logo.
	mail := mailer.Branded{Inner: mailer.New(cfg.Mail), Brand: func(ctx context.Context) (mailer.Brand, error) {
		b, err := branding.Load(ctx, pool, orgID)
		if err != nil {
			return mailer.Brand{}, err
		}
		mb := mailer.Brand{Name: b.Name, Color: b.Color(), SupportEmail: b.SupportEmail}
		if p := b.LogoPath(); p != "" {
			mb.LogoURL = strings.TrimRight(cfg.WebPublicURL, "/") + p
		}
		return mb, nil
	}}
	hooks := webhooks.NewWorker(pool, cfg.WebhooksAllowInsecure)
	if cfg.WebhooksAllowInsecure {
		slog.Warn("WEBHOOKS_ALLOW_INSECURE is on: webhooks may target http:// and private addresses")
	}
	go hooks.Run(ctx)
	portalHandler := &portal.Handler{
		DB: pool, Store: store, Mail: mail,
		WebURL: cfg.WebPublicURL, MaxUploadMB: cfg.MaxUploadMB, OrgID: orgID,
		Webhooks: hooks,
		Face:     face.New(cfg.FaceURL, cfg.FaceToken),
	}
	if portalHandler.Face == nil {
		slog.Warn("FACE_URL is not set: liveness fields are unavailable")
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: (&httpapi.Server{
			DB:           pool,
			WebOrigin:    cfg.WebOrigin,
			OrgID:        orgID,
			CookieSecure: cfg.CookieSecure,
			Files:        store,
			Portal:       portalHandler.Routes(),
			Links:        portalHandler.AdminRoutes(),
			Mail:         mail,
			WebURL:       cfg.WebPublicURL,
			Uploads:      store,
			Webhooks:     hooks,
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "addr", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
