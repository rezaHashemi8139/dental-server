package service

import (
	"context"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

type InstallationService struct {
	d *Deps
}

var _ dentalv1connect.InstallationServiceHandler = (*InstallationService)(nil)

func NewInstallationService(d *Deps) *InstallationService {
	return &InstallationService{d: d}
}

const maxMacAddresses = 32

func (s *InstallationService) Register(ctx context.Context, req *dentalv1.RegisterRequest) (*dentalv1.RegisterResponse, error) {
	m := req.GetMachine()
	if m == nil || len(m.GetMachineIdHash()) != 32 || len(m.GetMacAddresses()) > maxMacAddresses {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	secret := secure.RandomBytes(32)
	var id pgtype.UUID
	var known bool
	err := s.d.DB.QueryRow(ctx, `
		WITH previous AS (SELECT 1 FROM installations WHERE machine_id_hash = $1 LIMIT 1)
		INSERT INTO installations (secret_hash, machine_id_hash, hostname, os_name, os_version,
		                           cpu_model, cpu_cores, memory_bytes, mac_addresses, app_version)
		VALUES ($2, $1, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, EXISTS (SELECT 1 FROM previous)`,
		m.GetMachineIdHash(), secure.HashSecret(secret),
		limit(m.GetHostname(), 255), limit(m.GetOsName(), 255), limit(m.GetOsVersion(), 64),
		limit(m.GetCpuModel(), 255), int32(min(m.GetCpuCores(), 1<<16)), int64(min(m.GetMemoryBytes(), 1<<62)),
		m.GetMacAddresses(), limit(m.GetAppVersion(), 64),
	).Scan(&id, &known)
	if err != nil {
		return nil, internal(err)
	}

	return &dentalv1.RegisterResponse{
		InstallationId:     uuidBytes(id),
		InstallationSecret: secret,
		KnownMachine:       known,
		ServerTime:         timestamppb.New(s.d.Now()),
	}, nil
}

// limit cuts s to at most n bytes without splitting a UTF-8 character.
func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
