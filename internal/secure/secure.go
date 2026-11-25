// Package secure holds the server's cryptography: password hashing, encryption
// of stored keys, sealing keys to a client, signing offline grants and tokens.
// The desktop app implements the client side of SealedBox and the grant check;
// both must follow the scheme documented in proto/dental/v1/crypto.proto.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	dentalv1 "dental-server/gen/dental/v1"
)

// RandomBytes returns n bytes from the system CSPRNG.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // never fails (crypto/rand panics on failure)
	return b
}

// Wipe overwrites a secret in memory once it is no longer needed.
func Wipe(b []byte) {
	clear(b)
}

// ---------------------------------------------------------------------------
// Passwords: Argon2id, stored in the PHC string format.

const (
	argonMemoryKiB = 19 * 1024 // OWASP minimum recommendation for Argon2id
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

var b64 = base64.RawStdEncoding

func HashPassword(password string) string {
	salt := RandomBytes(argonSaltLen)
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(hash))
}

// dummyHash is checked when a user does not exist, so a login takes the same
// time whether or not the username is known.
var dummyHash = HashPassword("dummy password for timing")

// VerifyPassword reports whether password matches encoded. An empty encoded
// value (unknown user) is compared against a dummy hash and never matches.
func VerifyPassword(password, encoded string) bool {
	known := encoded != ""
	if !known {
		encoded = dummyHash
	}

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1 && known
}

// ---------------------------------------------------------------------------
// Keys at rest: AES-256-GCM with the server's key-encryption key.

type KeyWrapper struct {
	aead cipher.AEAD
}

func NewKeyWrapper(key []byte) (*KeyWrapper, error) {
	if len(key) != 32 {
		return nil, errors.New("key-encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &KeyWrapper{aead: aead}, nil
}

// Wrap encrypts plain, bound to aad. Output: nonce || ciphertext.
func (w *KeyWrapper) Wrap(plain, aad []byte) []byte {
	nonce := RandomBytes(w.aead.NonceSize())
	return w.aead.Seal(nonce, nonce, plain, aad)
}

func (w *KeyWrapper) Unwrap(data, aad []byte) ([]byte, error) {
	n := w.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("wrapped key too short")
	}
	return w.aead.Open(nil, data[:n], data[n:], aad)
}

// ---------------------------------------------------------------------------
// SealedBox: X25519 + HKDF-SHA256 + XChaCha20-Poly1305 (see crypto.proto).

const sealedBoxInfo = "dental/sealed-box/v1"

func sealedBoxKey(shared, clientPub, serverPub []byte) ([]byte, error) {
	salt := append(append([]byte{}, clientPub...), serverPub...)
	return hkdf.Key(sha256.New, shared, salt, sealedBoxInfo, chacha20poly1305.KeySize)
}

// Seal encrypts plain so only the holder of the private key for clientPublicKey can read it.
func Seal(clientPublicKey, plain, aad []byte) (*dentalv1.SealedBox, error) {
	clientPub, err := ecdh.X25519().NewPublicKey(clientPublicKey)
	if err != nil {
		return nil, fmt.Errorf("client public key: %w", err)
	}
	serverPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := serverPriv.ECDH(clientPub)
	if err != nil {
		return nil, fmt.Errorf("key agreement: %w", err)
	}
	defer Wipe(shared)

	serverPub := serverPriv.PublicKey().Bytes()
	key, err := sealedBoxKey(shared, clientPublicKey, serverPub)
	if err != nil {
		return nil, err
	}
	defer Wipe(key)

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := RandomBytes(aead.NonceSize())
	return &dentalv1.SealedBox{
		ServerPublicKey: serverPub,
		Nonce:           nonce,
		Ciphertext:      aead.Seal(nil, nonce, plain, aad),
	}, nil
}

// OpenSealed is the client side of Seal. The server never needs it; it exists for tests.
func OpenSealed(clientPrivate *ecdh.PrivateKey, box *dentalv1.SealedBox, aad []byte) ([]byte, error) {
	serverPub, err := ecdh.X25519().NewPublicKey(box.GetServerPublicKey())
	if err != nil {
		return nil, err
	}
	shared, err := clientPrivate.ECDH(serverPub)
	if err != nil {
		return nil, err
	}
	key, err := sealedBoxKey(shared, clientPrivate.PublicKey().Bytes(), box.GetServerPublicKey())
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, box.GetNonce(), box.GetCiphertext(), aad)
}

// ---------------------------------------------------------------------------
// Offline grants: Ed25519.

type Signer struct {
	key ed25519.PrivateKey
}

// NewSigner takes a 32-byte Ed25519 seed.
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key must be a 32-byte Ed25519 seed")
	}
	return &Signer{key: ed25519.NewKeyFromSeed(seed)}, nil
}

func (s *Signer) Sign(message []byte) []byte {
	return ed25519.Sign(s.key, message)
}

func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.key.Public().(ed25519.PublicKey)
}

// ---------------------------------------------------------------------------
// Tokens and secrets: random values the server only stores as SHA-256 hashes.

// NewToken returns a random session token and the hash to store.
func NewToken() (token string, hash []byte) {
	token = base64.RawURLEncoding.EncodeToString(RandomBytes(32))
	return token, HashToken(token)
}

func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func HashSecret(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// SecretMatches compares a secret with its stored hash in constant time.
func SecretMatches(secret, storedHash []byte) bool {
	return subtle.ConstantTimeCompare(HashSecret(secret), storedHash) == 1
}

// ---------------------------------------------------------------------------
// Admin tokens: JSON Web Tokens (RFC 7519) signed with HMAC-SHA256 (HS256).

// ErrInvalidToken is returned for a token that is malformed, not signed with
// this key, meant for another audience or expired.
var ErrInvalidToken = errors.New("invalid token")

// TokenClaims are the claims of a token issued by TokenSigner.
type TokenClaims struct {
	// Who the token is for, e.g. an administrator's id.
	Subject string `json:"sub"`
	// The server-side session the token belongs to, checked on every use so the
	// token stops working as soon as that session is revoked.
	SessionID string `json:"sid"`
	Audience  string `json:"aud"`
	// Seconds since the Unix epoch.
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// TokenSigner issues and verifies HS256 JSON Web Tokens.
type TokenSigner struct {
	key []byte
}

// tokenHeader is the only header TokenSigner writes or accepts, so a token can
// never choose its own algorithm (e.g. "none").
var tokenHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// NewTokenSigner takes a 32-byte HMAC key.
func NewTokenSigner(key []byte) (*TokenSigner, error) {
	if len(key) != 32 {
		return nil, errors.New("token key must be 32 bytes")
	}
	return &TokenSigner{key: key}, nil
}

func (s *TokenSigner) mac(signingInput string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(signingInput))
	return m.Sum(nil)
}

// Sign returns the token for claims.
func (s *TokenSigner) Sign(claims TokenClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := tokenHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(s.mac(signingInput)), nil
}

// Verify checks the token's signature, audience and expiry at now, and returns its claims.
func (s *TokenSigner) Verify(token, audience string, now time.Time) (TokenClaims, error) {
	header, rest, ok1 := strings.Cut(token, ".")
	payload, signature, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 || header != tokenHeader {
		return TokenClaims{}, ErrInvalidToken
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(got, s.mac(header+"."+payload)) {
		return TokenClaims{}, ErrInvalidToken
	}

	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	var claims TokenClaims
	if err := json.Unmarshal(data, &claims); err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	if claims.Audience != audience || now.Unix() >= claims.ExpiresAt {
		return TokenClaims{}, ErrInvalidToken
	}
	return claims, nil
}
