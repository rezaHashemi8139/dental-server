package statuspage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func get(t *testing.T, h *Handler) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Code, rec.Body.String()
}

func TestDatabaseDown(t *testing.T) {
	// Nothing listens on port 1.
	db, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	code, body := get(t, &Handler{DB: db, Version: "test", Started: time.Now()})
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	for _, want := range []string{"Something is wrong", "Cannot reach PostgreSQL"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, "nobody") {
		t.Error("page shows the raw database error although ShowErrors is false")
	}

	_, body = get(t, &Handler{DB: db, Version: "test", Started: time.Now(), ShowErrors: true})
	if !strings.Contains(body, "nobody") {
		t.Error("page does not show the raw database error although ShowErrors is true")
	}
}

func TestDatabaseNotReady(t *testing.T) {
	db, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	notReady := func() error { return errors.New("apply migrations: boom") }

	code, body := get(t, &Handler{DB: db, NotReady: notReady, Version: "test", Started: time.Now()})
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if !strings.Contains(body, "not ready yet") || strings.Contains(body, "boom") {
		t.Error("page should show the generic not-ready text, not the raw error")
	}

	_, body = get(t, &Handler{DB: db, NotReady: notReady, Version: "test", Started: time.Now(), ShowErrors: true})
	if !strings.Contains(body, "apply migrations: boom") {
		t.Error("page does not show the raw error although ShowErrors is true")
	}
}

func TestDatabaseUp(t *testing.T) {
	_ = godotenv.Load("../../.env")
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	code, body := get(t, &Handler{DB: db, Version: "test", Started: time.Now()})
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "All systems operational") {
		t.Error("page does not say all systems are operational")
	}
}
