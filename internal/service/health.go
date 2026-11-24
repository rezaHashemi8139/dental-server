// Package service implements the RPC services defined in proto/.
package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/releases"
)

type HealthService struct {
	db       *pgxpool.Pool
	version  string
	notReady func() error
	releases *releases.Store
}

var _ dentalv1connect.HealthServiceHandler = (*HealthService)(nil)

// NewHealthService: notReady reports why the database is not usable yet (nil = ready).
// A nil notReady means the database is always considered set up. A nil releases
// means no desktop app version is forced to update.
func NewHealthService(db *pgxpool.Pool, version string, notReady func() error, releases *releases.Store) *HealthService {
	return &HealthService{db: db, version: version, notReady: notReady, releases: releases}
}

func (s *HealthService) Check(ctx context.Context, _ *dentalv1.CheckRequest) (*dentalv1.CheckResponse, error) {
	res := &dentalv1.CheckResponse{
		Version:    s.version,
		ServerTime: timestamppb.Now(),
		DatabaseOk: s.databaseOK(ctx),
	}
	if release := s.releases.Current(); release != nil && release.MinimumVersion != (releases.Version{}) {
		res.MinimumAppVersion = release.MinimumVersion.String()
	}
	return res, nil
}

func (s *HealthService) databaseOK(ctx context.Context) bool {
	if s.notReady != nil && s.notReady() != nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.db.Ping(pingCtx) == nil
}
