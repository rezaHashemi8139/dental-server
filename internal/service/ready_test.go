package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

// While the database is not ready, every RPC but the health check fails with Unavailable.
func TestReadyInterceptor(t *testing.T) {
	db, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	notReady := func() error { return errors.New("database is down") }
	interceptors := connect.WithInterceptors(ReadyInterceptor(notReady))
	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewHealthServiceHandler(NewHealthService(db, "test", notReady, nil), interceptors))
	mux.Handle(dentalv1connect.NewAuthServiceHandler(NewAuthService(&Deps{}), interceptors))
	server := httptest.NewServer(mux)
	defer server.Close()

	health := dentalv1connect.NewHealthServiceClient(server.Client(), server.URL)
	res, err := health.Check(context.Background(), &dentalv1.CheckRequest{})
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	if res.GetDatabaseOk() {
		t.Error("database_ok = true while the database is not ready")
	}

	auth := dentalv1connect.NewAuthServiceClient(server.Client(), server.URL)
	_, err = auth.Login(context.Background(), &dentalv1.LoginRequest{})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("login: code = %v, want Unavailable (err: %v)", connect.CodeOf(err), err)
	}
}
