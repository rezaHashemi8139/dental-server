// Dental server: accounts, installations, database keys, patient records synced
// from the clinics, downloadable interface languages and updates for the desktop app.
// RPCs are defined in proto/ and served with ConnectRPC, which speaks the gRPC,
// gRPC-Web and Connect protocols over plain net/http.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/config"
	"dental-server/internal/database"
	"dental-server/internal/languages"
	"dental-server/internal/releases"
	"dental-server/internal/secure"
	"dental-server/internal/service"
	"dental-server/internal/statuspage"
)

// Set at build time: go build -ldflags "-X main.version=1.2.3"
var version = "0.1.0-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "create-admin" {
		if err := createAdmin(os.Args[2:]); err != nil {
			slog.Error("create-admin failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	started := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Does not wait for PostgreSQL: the server comes up anyway, keeps retrying in the
	// background and reports the problem on the status page until the database is ready.
	db, err := database.Start(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Pool.Close()

	keys, err := secure.NewKeyWrapper(cfg.KeyEncryptionKey)
	if err != nil {
		return err
	}
	signer, err := secure.NewSigner(cfg.SigningKey)
	if err != nil {
		return err
	}
	adminTokens, err := secure.NewTokenSigner(cfg.AdminTokenKey)
	if err != nil {
		return err
	}
	// Checked at startup: a broken language file must not ship.
	langs, err := languages.Load()
	if err != nil {
		return err
	}

	// Checked at startup too: a broken release.json must not ship. Reread when it changes.
	appReleases, err := releases.Open(cfg.UpdatesDir)
	if err != nil {
		return err
	}

	deps := &service.Deps{
		DB: db.Pool, Keys: keys, Signer: signer, AdminTokens: adminTokens, SessionTTL: cfg.SessionTTL, Now: time.Now,
	}

	// Outermost first: log everything, refuse desktop apps that must update first,
	// reject invalid input (before any database work), refuse calls while the
	// database is not ready, then check the session.
	interceptors := connect.WithInterceptors(
		loggingInterceptor(),
		service.AppVersionInterceptor(appReleases),
		service.ValidateInterceptor(),
		service.ReadyInterceptor(db.NotReady),
		service.SessionInterceptor(deps),
	)

	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewHealthServiceHandler(service.NewHealthService(db.Pool, version, db.NotReady, appReleases), interceptors))
	mux.Handle(dentalv1connect.NewInstallationServiceHandler(service.NewInstallationService(deps), interceptors))
	mux.Handle(dentalv1connect.NewAuthServiceHandler(service.NewAuthService(deps), interceptors))
	mux.Handle(dentalv1connect.NewMemberServiceHandler(service.NewMemberService(deps), interceptors))
	mux.Handle(dentalv1connect.NewSyncServiceHandler(service.NewSyncService(deps), interceptors))
	mux.Handle(dentalv1connect.NewPatientServiceHandler(service.NewPatientService(deps), interceptors))
	mux.Handle(dentalv1connect.NewLanguageServiceHandler(service.NewLanguageService(langs), interceptors))
	// The desktop app's updater: release info and installers (plain HTTP, no session).
	releases.Register(mux, appReleases)
	// The admin panel has its own sessions: AdminSessionInterceptor instead of SessionInterceptor.
	mux.Handle(dentalv1connect.NewAdminServiceHandler(service.NewAdminService(deps), connect.WithInterceptors(
		loggingInterceptor(),
		service.ValidateInterceptor(),
		service.ReadyInterceptor(db.NotReady),
		service.AdminSessionInterceptor(deps),
	)))
	// Status page for people and uptime monitors. Raw errors only without TLS (development).
	mux.Handle("GET /{$}", &statuspage.Handler{DB: db.Pool, NotReady: db.NotReady, Version: version, Started: started, ShowErrors: !cfg.TLS()})

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// gRPC needs HTTP/2. Over TLS it is negotiated; without TLS (development) allow h2c.
		Protocols: protocols(cfg.TLS()),
	}

	errs := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", cfg.Addr, "tls", cfg.TLS(), "version", version)
		if cfg.TLS() {
			errs <- server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errs <- server.ListenAndServe()
		}
	}()

	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

func protocols(tls bool) *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	if tls {
		p.SetHTTP2(true)
	} else {
		p.SetUnencryptedHTTP2(true)
	}
	return p
}

// loggingInterceptor logs every RPC with its duration and error code.
func loggingInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			res, err := next(ctx, req)
			attrs := []any{"procedure", req.Spec().Procedure, "duration", time.Since(start)}
			if err != nil {
				slog.Warn("rpc failed", append(attrs, "code", connect.CodeOf(err), "error", err)...)
			} else {
				slog.Info("rpc", attrs...)
			}
			return res, err
		}
	}
}
