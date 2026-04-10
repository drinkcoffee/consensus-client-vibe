package engine

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTProvider generates and caches HS256 JWT tokens for Engine API authentication.
// The Engine API spec requires a shared 32-byte secret and an "iat" (issued-at) claim.
// Tokens are considered valid for 60 seconds; this provider refreshes 10 seconds early.
type JWTProvider struct {
	secret []byte

	mu          sync.Mutex
	cachedToken string
	expiresAt   time.Time
}

// NewJWTProvider reads the hex-encoded secret from path, validates it, and returns
// a ready-to-use provider. The file may contain an optional "0x" prefix and leading/
// trailing whitespace.
func NewJWTProvider(secretPath string) (*JWTProvider, error) {
	raw, err := os.ReadFile(secretPath)
	if err != nil {
		return nil, fmt.Errorf("read JWT secret file %q: %w", secretPath, err)
	}

	hexStr := strings.TrimSpace(string(raw))
	hexStr = strings.TrimPrefix(hexStr, "0x")

	secret, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("decode JWT secret: %w", err)
	}
	if len(secret) != 32 {
		return nil, fmt.Errorf("JWT secret must be exactly 32 bytes, got %d", len(secret))
	}

	return &JWTProvider{secret: secret}, nil
}

// Token returns a valid JWT bearer token, generating a new one if the cached token
// has expired or is within 10 seconds of expiry.
func (p *JWTProvider) Token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Now().Before(p.expiresAt.Add(-10 * time.Second)) {
		return p.cachedToken, nil
	}

	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iat": now.Unix(),
	})

	signed, err := tok.SignedString(p.secret)
	if err != nil {
		return "", fmt.Errorf("sign JWT token: %w", err)
	}

	p.cachedToken = signed
	p.expiresAt = now.Add(60 * time.Second)
	return signed, nil
}
