package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: 64 MiB, 2 passes, 1 lane (OWASP's second option).
const (
	argonMemory  = 64 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

var b64 = base64.RawStdEncoding

// HashPassword returns an argon2id PHC string for MAILPILOT_WEB_PASSWORD_HASH.
func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("le mot de passe doit faire au moins 12 caractères")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a PHC string in constant time.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("hash argon2id invalide")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("version argon2 non prise en charge")
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errors.New("paramètres argon2 invalides")
	}
	if m > 1<<20 || t > 10 || p == 0 || p > 16 { // refuse absurd costs from a tampered hash
		return false, errors.New("paramètres argon2 hors limites")
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("sel argon2 invalide")
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("hash argon2 invalide")
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want))) //nolint:gosec // len checked above
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// newToken returns 32 random bytes, hex encoded.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// tokenHash is what the database stores for a session cookie.
func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// limiter slows down password guessing: at most maxFails failures per key
// (client address) in window, and a global ceiling for distributed tries.
type limiter struct {
	mu       sync.Mutex
	fails    map[string][]time.Time
	global   []time.Time
	maxFails int
	maxAll   int
	window   time.Duration
}

func newLimiter() *limiter {
	return &limiter{fails: map[string][]time.Time{}, maxFails: 5, maxAll: 30, window: 15 * time.Minute}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// allowed reports whether key may try a password now.
func (l *limiter) allowed(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	l.fails[key] = prune(l.fails[key], cutoff)
	if len(l.fails[key]) == 0 {
		delete(l.fails, key)
	}
	l.global = prune(l.global, cutoff)
	return len(l.fails[key]) < l.maxFails && len(l.global) < l.maxAll
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.fails[key], now)
	l.global = append(l.global, now)
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
