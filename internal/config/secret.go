package config

import "log/slog"

// Secret holds a credential. Every printing path (fmt, slog, JSON) shows a
// placeholder; only Reveal returns the value.
type Secret struct{ v string }

// NewSecret wraps a credential.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the raw value. Call it only at the point of use.
func (s Secret) Reveal() string { return s.v }

// Empty reports whether no value was configured.
func (s Secret) Empty() bool { return s.v == "" }

const redacted = "[REDACTED]"

func (s Secret) String() string   { return s.mask() }
func (s Secret) GoString() string { return s.mask() }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.mask()) }

// MarshalJSON never exposes the value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.mask() + `"`), nil }

// MarshalText never exposes the value.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.mask()), nil }

func (s Secret) mask() string {
	if s.v == "" {
		return ""
	}
	return redacted
}
