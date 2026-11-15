// Dental server: accounts, installations and database keys for the desktop app.
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
	"dental-server/internal/secure"
	"dental-server/internal/service"
)

// Set at build time: go build -ldflags "-X main.version=1.2.3"
var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	keys, err := secure.NewKeyWrapper(cfg.KeyEncryptionKey)
	if err != nil {
		return err
	}
	signer, err := secure.NewSigner(cfg.SigningKey)
	if err != nil {
		return err
	}
	deps := &service.Deps{DB: db, Keys: keys, Signer: signer, SessionTTL: cfg.SessionTTL, Now: time.Now}

	interceptors := connect.WithInterceptors(loggingInterceptor(), service.SessionInterceptor(deps))

	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewHealthServiceHandler(service.NewHealthService(db, version), interceptors))
	mux.Handle(dentalv1connect.NewInstallationServiceHandler(service.NewInstallationService(deps), interceptors))
	mux.Handle(dentalv1connect.NewAuthServiceHandler(service.NewAuthService(deps), interceptors))
	mux.Handle(dentalv1connect.NewMemberServiceHandler(service.NewMemberService(deps), interceptors))

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
