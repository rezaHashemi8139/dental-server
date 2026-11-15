// Package statuspage serves a small HTML page at / that shows whether the server
// and its database are healthy. It answers 503 when something is wrong, so uptime
// monitors can watch it too.
package statuspage

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed status.html
var pageHTML string

var page = template.Must(template.New("status").Parse(pageHTML))

type Handler struct {
	DB      *pgxpool.Pool
	Version string
	Started time.Time
	// ShowErrors puts raw database errors on the page. Development only:
	// they name hosts and users, so in production the page stays generic.
	ShowErrors bool
}

type check struct {
	Name   string
	OK     bool
	Detail string
}

type pageData struct {
	OK      bool
	Checks  []check
	Version string
	Uptime  string
	Time    string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data := pageData{
		Checks: []check{
			{Name: "Server", OK: true, Detail: "Running"},
			h.checkDatabase(r.Context()),
		},
		Version: h.Version,
		Uptime:  time.Since(h.Started).Round(time.Second).String(),
		Time:    time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
	}
	data.OK = true
	for _, c := range data.Checks {
		data.OK = data.OK && c.OK
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !data.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if err := page.Execute(w, data); err != nil {
		slog.Error("render status page", "error", err)
	}
}

func (h *Handler) checkDatabase(ctx context.Context) check {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	err := h.DB.Ping(ctx)
	if err == nil {
		return check{Name: "Database", OK: true, Detail: fmt.Sprintf("Connected (%d ms)", time.Since(start).Milliseconds())}
	}

	slog.Warn("status page: database unreachable", "error", err)
	detail := "Cannot reach PostgreSQL. See the server log for details."
	if h.ShowErrors {
		detail = err.Error()
	}
	return check{Name: "Database", OK: false, Detail: detail}
}
