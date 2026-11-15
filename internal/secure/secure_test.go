package secure

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestPassword(t *testing.T) {
	hash := HashPassword("correct horse battery")
	if !VerifyPassword("correct horse battery", hash) {
		t.Error("correct password rejected")
	}
	if VerifyPassword("wrong password", hash) {
		t.Error("wrong password accepted")
	}
	if VerifyPassword("dummy password for timing", "") {
		t.Error("unknown user accepted")
	}
	if HashPassword("same") == HashPassword("same") {
		t.Error("hashes are not salted")
	}
}

func TestKeyWrapper(t *testing.T) {
	w, err := NewKeyWrapper(RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	secret := RandomBytes(32)
	wrapped := w.Wrap(secret, []byte("installation-a"))

	got, err := w.Unwrap(wrapped, []byte("installation-a"))
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("unwrap = %x, %v", got, err)
	}
	if _, err := w.Unwrap(wrapped, []byte("installation-b")); err == nil {
		t.Error("key unwrapped for another installation")
	}
}

func TestSealedBox(t *testing.T) {
	client, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("installation")
	box, err := Seal(client.PublicKey().Bytes(), []byte("database keys"), aad)
	if err != nil {
		t.Fatal(err)
	}

	plain, err := OpenSealed(client, box, aad)
	if err != nil || string(plain) != "database keys" {
		t.Fatalf("open = %q, %v", plain, err)
	}

	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err := OpenSealed(other, box, aad); err == nil {
		t.Error("another key opened the box")
	}
	if _, err := OpenSealed(client, box, []byte("other installation")); err == nil {
		t.Error("box opened with the wrong AAD")
	}
}

// Fixed vector shared with the desktop app's tests, so both sides provably derive the same key.
func TestSealedBoxKeyVector(t *testing.T) {
	shared := bytes.Repeat([]byte{1}, 32)
	clientPub := bytes.Repeat([]byte{2}, 32)
	serverPub := bytes.Repeat([]byte{3}, 32)
	key, err := sealedBoxKey(shared, clientPub, serverPub)
	if err != nil {
		t.Fatal(err)
	}
	const want = "77defcb63219db93339edc125d4289adb3e97e0eceb698e237e137f222e0a387"
	if got := hex.EncodeToString(key); got != want {
		t.Fatalf("sealed box key = %s, want %s", got, want)
	}
}

func TestSigner(t *testing.T) {
	s, err := NewSigner(RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	sig := s.Sign([]byte("grant"))
	if !ed25519.Verify(s.PublicKey(), []byte("grant"), sig) {
		t.Error("signature does not verify")
	}
	if ed25519.Verify(s.PublicKey(), []byte("changed"), sig) {
		t.Error("signature verifies for another message")
	}
}

func TestTokens(t *testing.T) {
	token, hash := NewToken()
	if !bytes.Equal(HashToken(token), hash) {
		t.Error("token hash mismatch")
	}
	secret := RandomBytes(32)
	if !SecretMatches(secret, HashSecret(secret)) || SecretMatches(RandomBytes(32), HashSecret(secret)) {
		t.Error("secret comparison wrong")
	}
}
