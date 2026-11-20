package access

import (
	"net/netip"
	"sync"
	"time"
)

// ClientKey is the identity limits and sessions are keyed on: the IPv4
// address, or the /64 prefix for IPv6 (one host controls a whole /64).
// Unparseable input is returned unchanged.
func ClientKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

type ipState struct {
	winStart time.Time // start of the current 1-minute window
	winCount int
	day      time.Time // UTC midnight of the current day counter
	dayCount int
	last     time.Time // last request time, for Sweep
}

// IPLimiter is a per-IP limiter: a fixed 1-minute window plus a per-day counter
// that resets at the UTC day boundary. Refused requests are not counted.
type IPLimiter struct {
	mu     sync.Mutex
	perMin int
	perDay int
	ips    map[string]*ipState
}

func NewIPLimiter(perMin, perDay int) *IPLimiter {
	return &IPLimiter{perMin: perMin, perDay: perDay, ips: make(map[string]*ipState)}
}

// Allow reports whether ip may make a request at now. When refused, retryAfter
// is how long until the blocking window resets (at least 1s).
func (l *IPLimiter) Allow(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := now.UTC()
	today := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	key := ClientKey(ip)
	s := l.ips[key]
	if s == nil {
		s = &ipState{winStart: now, day: today}
		l.ips[key] = s
	}
	s.last = now
	if !s.day.Equal(today) {
		s.day, s.dayCount = today, 0
	}
	if now.Sub(s.winStart) >= time.Minute || now.Before(s.winStart) {
		s.winStart, s.winCount = now, 0
	}
	if l.perDay > 0 && s.dayCount >= l.perDay {
		return false, atLeastSecond(today.Add(24 * time.Hour).Sub(now))
	}
	if l.perMin > 0 && s.winCount >= l.perMin {
		return false, atLeastSecond(s.winStart.Add(time.Minute).Sub(now))
	}
	s.winCount++
	s.dayCount++
	return true, 0
}

func atLeastSecond(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	return d
}

// Sweep drops IPs idle for more than a day so the map cannot grow without bound.
func (l *IPLimiter) Sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, s := range l.ips {
		if now.Sub(s.last) > 24*time.Hour {
			delete(l.ips, ip)
		}
	}
}
