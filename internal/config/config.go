// Package config reads server settings from environment variables.
// In development they come from the .env file next to the binary (see .env.example).
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Address to listen on, e.g. ":8080".
	Addr string
	// PostgreSQL connection string.
	DatabaseURL string
	// Encrypts local-database keys stored in PostgreSQL (32 bytes).
	KeyEncryptionKey []byte
	// Ed25519 seed that signs offline grants (32 bytes).
	SigningKey []byte
	// How long a login session lasts.
	SessionTTL time.Duration
	// TLS certificate and key. Both empty = plain HTTP (development only).
	TLSCertFile string
	TLSKeyFile  string
	// Folder with the desktop app release on offer (release.json + installer), see internal/releases.
	UpdatesDir string
}

func Load() (Config, error) {
	// A missing .env is fine: in production the variables come from the environment.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}

	cfg := Config{
		Addr:        getenv("ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		TLSCertFile: os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:  os.Getenv("TLS_KEY_FILE"),
		UpdatesDir:  getenv("UPDATES_DIR", "updates"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is not set")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return Config{}, errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}

	var err error
	if cfg.KeyEncryptionKey, err = base64Key("KEY_ENCRYPTION_KEY"); err != nil {
		return Config{}, err
	}
	if cfg.SigningKey, err = base64Key("SIGNING_KEY"); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = time.ParseDuration(getenv("SESSION_TTL", "12h")); err != nil || cfg.SessionTTL <= 0 {
		return Config{}, fmt.Errorf("SESSION_TTL: invalid duration %q", os.Getenv("SESSION_TTL"))
	}
	return cfg, nil
}

// base64Key reads a required 32-byte key given in base64.
func base64Key(name string) ([]byte, error) {
	value := os.Getenv(name)
	if value == "" {
		return nil, fmt.Errorf("%s is not set", name)
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be 32 bytes in base64", name)
	}
	return key, nil
}

// TLS reports whether the server should serve HTTPS.
func (c Config) TLS() bool {
	return c.TLSCertFile != ""
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
