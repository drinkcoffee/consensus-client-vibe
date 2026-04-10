package engine

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func writeTempSecret(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "jwt.hex")
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNewJWTProvider_ValidSecret(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	path := writeTempSecret(t, hex.EncodeToString(secret))

	p, err := NewJWTProvider(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestNewJWTProvider_WithOxPrefix(t *testing.T) {
	secret := make([]byte, 32)
	path := writeTempSecret(t, "0x"+hex.EncodeToString(secret))

	_, err := NewJWTProvider(path)
	if err != nil {
		t.Fatalf("unexpected error with 0x prefix: %v", err)
	}
}

func TestNewJWTProvider_WrongLength(t *testing.T) {
	path := writeTempSecret(t, hex.EncodeToString([]byte("tooshort")))
	_, err := NewJWTProvider(path)
	if err == nil {
		t.Fatal("expected error for short secret")
	}
}

func TestNewJWTProvider_InvalidHex(t *testing.T) {
	path := writeTempSecret(t, "not-valid-hex!!")
	_, err := NewJWTProvider(path)
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

func TestNewJWTProvider_MissingFile(t *testing.T) {
	_, err := NewJWTProvider("/nonexistent/path/jwt.hex")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestToken_ValidAndParseable(t *testing.T) {
	secret := make([]byte, 32)
	path := writeTempSecret(t, hex.EncodeToString(secret))

	p, err := NewJWTProvider(path)
	if err != nil {
		t.Fatal(err)
	}

	tok, err := p.Token()
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}

	// Parse and verify the token has the expected iat claim.
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return secret, nil
	})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("expected MapClaims")
	}
	iat, ok := claims["iat"]
	if !ok {
		t.Fatal("missing iat claim")
	}
	iatTime := time.Unix(int64(iat.(float64)), 0)
	if time.Since(iatTime) > 5*time.Second {
		t.Errorf("iat claim too old: %v", iatTime)
	}
}

func TestToken_Cached(t *testing.T) {
	secret := make([]byte, 32)
	path := writeTempSecret(t, hex.EncodeToString(secret))

	p, err := NewJWTProvider(path)
	if err != nil {
		t.Fatal(err)
	}

	tok1, err := p.Token()
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := p.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != tok2 {
		t.Error("expected the same token to be returned when not yet expired")
	}
}
