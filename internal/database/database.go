// Package database connects to PostgreSQL and keeps its schema up to date.
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Schema changes live in migrations/ as numbered SQL files and are applied on startup.
//
//go:embed migrations/*.sql
var migrations embed.FS

// Open connects to PostgreSQL and applies any pending migrations, or fails right away.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := setup(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// DB is a connection pool that may not be usable yet. Start returns it at once and
// keeps trying in the background until PostgreSQL is reachable and migrated, so the
// server can come up (and report the problem) while the database is down.
type DB struct {
	Pool *pgxpool.Pool

	mu       sync.Mutex
	notReady error
}

// Start creates the pool and begins getting it ready in the background until ctx ends.
// It only fails on an invalid connection string.
func Start(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	db := &DB{Pool: pool, notReady: errors.New("connecting to the database")}
	go db.prepare(ctx)
	return db, nil
}

// NotReady returns why the database cannot be used yet, or nil once it is reachable and migrated.
func (d *DB) NotReady() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.notReady
}

func (d *DB) prepare(ctx context.Context) {
	const maxDelay = 30 * time.Second
	delay := time.Second
	for {
		err := setup(ctx, d.Pool)
		d.mu.Lock()
		d.notReady = err
		d.mu.Unlock()
		if err == nil {
			slog.Info("database ready")
			return
		}
		if ctx.Err() != nil {
			return
		}

		slog.Warn("database not ready, will retry", "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}

// setup checks that PostgreSQL answers, then applies pending migrations.
func setup(ctx context.Context, pool *pgxpool.Pool) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("reach database: %w", err)
	}
	return migrate(ctx, pool)
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, mustSub(migrations, "migrations"))
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
