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
	db      *pgxpool.Pool
	version string
}

var _ dentalv1connect.HealthServiceHandler = (*HealthService)(nil)

func NewHealthService(db *pgxpool.Pool, version string) *HealthService {
	return &HealthService{db: db, version: version}
}

func (s *HealthService) Check(ctx context.Context, _ *dentalv1.CheckRequest) (*dentalv1.CheckResponse, error) {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	return &dentalv1.CheckResponse{
		Version:    s.version,
		ServerTime: timestamppb.Now(),
		DatabaseOk: s.db.Ping(pingCtx) == nil,
	}, nil
}
