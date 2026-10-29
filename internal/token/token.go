// Package token mints and verifies the two kinds of credential this API uses.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// inviteAlphabet is Crockford-style base32 without the letters that get
// misread when a guest reads a link aloud or retypes it from a screenshot:
// no I, L, O or U. 16 characters of it is 80 bits of entropy.
const inviteAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var inviteEncoding = base32.NewEncoding(inviteAlphabet).WithPadding(base32.NoPadding)

// NewInvite returns an unguessable per-guest token for their personal link.
//
// 10 random bytes -> 16 characters. This token *is* the guest's credential, in
// the same way a calendar invite link or a wedding RSVP link is: whoever opens
// it is treated as that guest. That is the right trade for an event companion
// -- no passwords for a hundred guests who use the app for one day -- but it
// means the link must only ever travel over the guest's own WhatsApp thread,
// and it must be revocable, which is why the store keeps a `revoked` flag.
func NewInvite() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return inviteEncoding.EncodeToString(b), nil
}

// ValidInviteShape reports whether s could be a token at all.
//
// Checked before touching the database so a flood of junk paths costs nothing,
// and so the rate limiter is not spent on obvious noise.
func ValidInviteShape(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(inviteAlphabet, r) {
			return false
		}
	}
	return true
}

// Session is a signed, expiring bearer token for the admin and stage sides.
//
// Self-contained on purpose: the committee's laptop and the MC's phone can
// keep working through a Redis restart, because verifying a session needs
// nothing but the secret.
type Session struct {
	Subject string // guest/admin/person id
	Role    string // "admin" | "mc" | "pembicara"
	Expires time.Time
}

// Sign renders a session as "<base64url payload>.<base64url hmac>".
func Sign(secret string, s Session) string {
	payload := fmt.Sprintf("%s:%s:%d", s.Subject, s.Role, s.Expires.Unix())
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enc + "." + sign(secret, enc)
}

// Verify checks the signature and the expiry, in that order.
func Verify(secret, raw string) (Session, error) {
	enc, mac, ok := strings.Cut(raw, ".")
	if !ok {
		return Session{}, fmt.Errorf("malformed token")
	}
	// Constant time: a timing-distinguishable compare here would leak the
	// signature one byte at a time.
	if subtle.ConstantTimeCompare([]byte(mac), []byte(sign(secret, enc))) != 1 {
		return Session{}, fmt.Errorf("bad signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Session{}, fmt.Errorf("bad payload")
	}
	parts := strings.Split(string(payload), ":")
	if len(parts) != 3 {
		return Session{}, fmt.Errorf("bad payload")
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Session{}, fmt.Errorf("bad expiry")
	}
	exp := time.Unix(unix, 0)
	if time.Now().After(exp) {
		return Session{}, fmt.Errorf("expired")
	}
	return Session{Subject: parts[0], Role: parts[1], Expires: exp}, nil
}

func sign(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// NewSecret is used when SESSION_SECRET is unset: good enough to run, but it
// changes on every restart, which signs everybody out.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A process that cannot read the OS random source must not fall back
		// to something predictable and keep signing tokens.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
