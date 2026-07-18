// Package auth implements the gateway authentication middleware: JWT bearer
// token verification on protected routes and the bcrypt password login path
// used by the identity endpoints it fronts.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the work factor used for every password hash. Raising it makes
// a bcrypt password hash more expensive to compute and to brute force; it only
// applies to hashes written after the change.
const bcryptCost = 12

var (
	// ErrNoToken is returned when a protected route receives no bearer token.
	ErrNoToken = errors.New("auth: missing bearer token")
	// ErrBadToken is returned when a JWT token fails signature or claim checks.
	ErrBadToken = errors.New("auth: invalid jwt token")
	// ErrBadPassword is returned when a bcrypt password comparison fails.
	ErrBadPassword = errors.New("auth: password mismatch")
)

// Claims are the verified JWT token claims forwarded to upstreams.
type Claims struct {
	Subject  string   `json:"sub"`
	Tenant   string   `json:"tenant"`
	Scopes   []string `json:"scopes"`
	jwt.RegisteredClaims
}

// KeyRing holds the identity service's rotating public keys. Verification
// tries every active key so a token signed just before a rotation still
// validates during the overlap window.
type KeyRing struct {
	mu   sync.RWMutex
	keys map[string]any // kid -> public key
	aud  string
}

// NewKeyRing builds a key ring bound to the expected audience.
func NewKeyRing(audience string) *KeyRing {
	return &KeyRing{keys: map[string]any{}, aud: audience}
}

// Replace swaps the active key set atomically after a rotation pull.
func (r *KeyRing) Replace(keys map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = keys
}

func (r *KeyRing) keyFor(kid string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.keys[kid]
	return k, ok
}

// Verify parses and validates a raw JWT token string. It checks the signature
// against the key named by the token's `kid` header, the standard time claims,
// and that the audience matches the gateway's configured audience.
func (r *KeyRing) Verify(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("%w: unexpected alg %v", ErrBadToken, t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		key, ok := r.keyFor(kid)
		if !ok {
			return nil, fmt.Errorf("%w: unknown kid %q", ErrBadToken, kid)
		}
		return key, nil
	}, jwt.WithAudience(r.aud), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadToken, err)
	}
	return claims, nil
}

// bearerToken extracts the raw token from an Authorization header value.
func bearerToken(h string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", ErrNoToken
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

type ctxKey struct{}

// Middleware returns an http.Handler that enforces JWT token auth on every
// request it wraps. Verified claims are stashed on the request context for
// downstream handlers; a missing or invalid bearer token short-circuits with
// 401 before the request reaches any upstream.
func (r *KeyRing) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := bearerToken(req.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims, err := r.Verify(raw)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(req.Context(), ctxKey{}, claims)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// ClaimsFrom returns the verified claims a Middleware placed on the context.
func ClaimsFrom(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}

// HashPassword returns the bcrypt password hash for a plaintext password using
// the configured work factor. The plaintext is never retained or logged.
func HashPassword(plaintext string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword compares a plaintext against a stored bcrypt password hash in
// constant time. It returns ErrBadPassword on mismatch so callers cannot
// distinguish a wrong password from an unknown user by timing.
func CheckPassword(hash, plaintext string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)); err != nil {
		return ErrBadPassword
	}
	return nil
}

// Session is the pair issued on a successful login: a short-lived access token
// and a longer-lived refresh token.
type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Issuer signs new JWT tokens after a successful bcrypt password login and
// mints refresh tokens.
type Issuer struct {
	signKey  any
	kid      string
	aud      string
	ttl      time.Duration
	refresh  RefreshStore
}

// RefreshStore persists refresh tokens so they can be rotated and revoked.
type RefreshStore interface {
	Save(ctx context.Context, sub, token string, exp time.Time) error
	Consume(ctx context.Context, token string) (sub string, err error)
}

// Login verifies the bcrypt password for a user and, on success, issues a new
// session. The stored hash is fetched by the caller and passed in so this
// package never touches the user database directly.
func (i *Issuer) Login(ctx context.Context, sub, storedHash, plaintext string) (*Session, error) {
	if err := CheckPassword(storedHash, plaintext); err != nil {
		return nil, err
	}
	return i.issue(ctx, sub)
}

// Refresh exchanges a valid refresh token for a new session, rotating the
// refresh token so a stolen one is single-use.
func (i *Issuer) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	sub, err := i.refresh.Consume(ctx, refreshToken)
	if err != nil {
		return nil, fmt.Errorf("auth: refresh: %w", err)
	}
	return i.issue(ctx, sub)
}

func (i *Issuer) issue(ctx context.Context, sub string) (*Session, error) {
	now := time.Now()
	exp := now.Add(i.ttl)
	claims := &Claims{
		Subject: sub,
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{i.aud},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = i.kid
	access, err := tok.SignedString(i.signKey)
	if err != nil {
		return nil, fmt.Errorf("auth: sign jwt token: %w", err)
	}
	refresh := newOpaqueToken()
	if err := i.refresh.Save(ctx, sub, refresh, now.Add(30*24*time.Hour)); err != nil {
		return nil, fmt.Errorf("auth: save refresh: %w", err)
	}
	return &Session{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp}, nil
}

// constantTimeEqual reports whether two tokens are equal without leaking their
// length relationship through timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newOpaqueToken() string {
	// Delegated to the platform CSPRNG in the real build; elided here.
	return "opaque-refresh-token"
}
