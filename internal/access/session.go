// Package access holds the access-control building blocks for the public API.
package access

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie set after a successful Turnstile check.
const CookieName = "ss"

// Sessions issues and validates stateless signed session cookies.
// Cookie value: <expiryUnix>.<base64url(HMAC-SHA256(secret, expiryUnix))>.
type Sessions struct {
	secret []byte
	ttl    time.Duration
}

func NewSessions(secret []byte, ttl time.Duration) *Sessions {
	return &Sessions{secret: secret, ttl: ttl}
}

// sign binds the expiry to the client (ClientKey), so a cookie solved once
// cannot be shared across proxy IPs.
func (s *Sessions) sign(exp, client string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(exp + "|" + ClientKey(client)))
	return m.Sum(nil)
}

// Issue sets the session cookie for client (the caller's IP). host is the
// request Host; the Secure flag is omitted for localhost/127.0.0.1 so dev over
// plain http works.
func (s *Sessions) Issue(w http.ResponseWriter, host, client string, now time.Time) {
	exp := now.Add(s.ttl)
	e := strconv.FormatInt(exp.Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    e + "." + base64.RawURLEncoding.EncodeToString(s.sign(e, client)),
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(s.ttl.Seconds()),
		HttpOnly: true,
		Secure:   !isLocalHost(host),
		SameSite: http.SameSiteLaxMode,
	})
}

// Valid reports whether the request carries an unexpired cookie signed for client.
func (s *Sessions) Valid(r *http.Request, client string, now time.Time) bool {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	e, sig, ok := strings.Cut(c.Value, ".")
	if !ok || e == "" || sig == "" {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(e, client)) {
		return false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() < exp
}

func isLocalHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}
