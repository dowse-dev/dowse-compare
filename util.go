package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dowse-compare/internal/access"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// clientIP prefers Cloudflare's CF-Connecting-IP, then the connection address.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// isLocalRequest reports whether r was made on this machine (local testing),
// not through Caddy or Cloudflare, which always add X-Forwarded-For or CF-Connecting-IP.
func isLocalRequest(r *http.Request) bool {
	if r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sessionAllowed: no gate, a local request, or a valid human-check session.
func sessionAllowed(sessions *access.Sessions, r *http.Request, now time.Time) bool {
	return sessions == nil || isLocalRequest(r) || sessions.Valid(r, clientIP(r), now)
}

const maxURLChars = 2000

// checkURL returns why a page URL is refused ("" when it is fine): only
// http(s) addresses on the public internet.
func checkURL(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case len(raw) > maxURLChars:
		return fmt.Sprintf("url is too long (max %d characters)", maxURLChars)
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return "invalid URL: use an http or https address"
	case isInternalHost(u.Hostname()):
		return "invalid URL: private and internal addresses are not allowed"
	}
	return ""
}

// isInternalHost reports whether a host name or IP literal points inside a
// network: localhost, *.local / *.internal names, and loopback, private,
// link-local or unspecified addresses.
func isInternalHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" || h == "localhost" || (!strings.Contains(h, ".") && net.ParseIP(h) == nil) {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".localdomain", ".lan", ".home.arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || isCGNAT(ip)
	}
	return false
}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
}
