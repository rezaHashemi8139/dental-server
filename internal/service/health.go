// Package service implements the RPC services defined in proto/.
package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

type HealthService struct {
	db       *pgxpool.Pool
	version  string
	notReady func() error
}

var _ dentalv1connect.HealthServiceHandler = (*HealthService)(nil)

// NewHealthService: notReady reports why the database is not usable yet (nil = ready).
// A nil notReady means the database is always considered set up.
func NewHealthService(db *pgxpool.Pool, version string, notReady func() error) *HealthService {
	return &HealthService{db: db, version: version, notReady: notReady}
}

func (s *HealthService) Check(ctx context.Context, _ *dentalv1.CheckRequest) (*dentalv1.CheckResponse, error) {
	return &dentalv1.CheckResponse{
		Version:    s.version,
		ServerTime: timestamppb.Now(),
		DatabaseOk: s.databaseOK(ctx),
	}, nil
}

func (s *HealthService) databaseOK(ctx context.Context) bool {
	if s.notReady != nil && s.notReady() != nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.db.Ping(pingCtx) == nil
}
