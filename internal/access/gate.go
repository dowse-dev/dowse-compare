package access

// Gate caps the number of searches running at once.
// It is a buffered channel of held slots: TryAcquire sends, Release does a
// non-blocking receive. A Release with no slot held is a no-op, so a stray
// extra Release can never raise capacity above max or block. Callers should
// still pair each successful TryAcquire with exactly one Release (use defer),
// since an extra Release while other slots are held would free one of theirs.
type Gate struct {
	slots chan struct{}
}

func NewGate(max int) *Gate {
	if max < 1 {
		max = 1
	}
	return &Gate{slots: make(chan struct{}, max)}
}

func (g *Gate) TryAcquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gate) Release() {
	select {
	case <-g.slots:
	default:
	}
}

func (g *Gate) InFlight() int { return len(g.slots) }
