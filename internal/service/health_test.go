package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

// Calls Check with a real gRPC client (binary protobuf over HTTP/2 + TLS),
// the same protocol the desktop app uses.
func TestHealthCheckOverGRPC(t *testing.T) {
	// Nothing listens on port 1, so the database check must report false without failing the RPC.
	db, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewHealthServiceHandler(NewHealthService(db, "test", nil, nil)))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	client := dentalv1connect.NewHealthServiceClient(server.Client(), server.URL, connect.WithGRPC())
	before := time.Now()
	res, err := client.Check(context.Background(), &dentalv1.CheckRequest{})
	if err != nil {
		t.Fatal(err)
	}

	if res.GetVersion() != "test" {
		t.Errorf("version = %q, want %q", res.GetVersion(), "test")
	}
	if res.GetDatabaseOk() {
		t.Error("database_ok = true, want false for an unreachable database")
	}
	if got := res.GetServerTime().AsTime(); got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("server_time = %v, not close to now", got)
	}
}
