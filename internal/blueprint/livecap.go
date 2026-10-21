package blueprint

import "sync"

// liveCap is a blueprint's cost cap as its runs in flight read it: seeded
// from the store, then moved only by a cap reply, so a run's sink reads the
// cap in force at every usage line.
type liveCap struct {
	mu     sync.Mutex
	usd    float64
	seeded bool
}

// get is the cap, 0 meaning none.
func (l *liveCap) get() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.usd
}

// seed sets the cap read from the store, only when nothing seeded or set it
// yet: a read that raced a reply may be older than the cap it set.
func (l *liveCap) seed(usd float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.seeded {
		l.usd, l.seeded = usd, true
	}
}

// set runs write, which stores usd, and takes usd only when it succeeds. mu
// is held across write, so two cap replies cannot leave the cell and the
// store disagreeing; a sink's get waits out one store write at most.
func (l *liveCap) set(usd float64, write func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := write(); err != nil {
		return err
	}

	l.usd, l.seeded = usd, true

	return nil
}

// capRead is a run's limit as read at one moment. capped false means
// nothing bounds the run; blueprintUSD is the blueprint cap read (0: none);
// bound means the blueprint's remainder is the run cap, so reaching it is a
// cap stop.
type capRead struct {
	usd          float64
	capped       bool
	blueprintUSD float64
	bound        bool
}

// runLimit is what bounds one run, immutable once built: the per-run cap
// (0: none), the blueprint's live cap less baseUSD, what the ledger had
// spent when the run was allowed, and what the asking run's sink has left
// when this run serves one.
type runLimit struct {
	perRunUSD float64
	blueprint *liveCap
	baseUSD   float64
	parent    *harnessSink
}

// read is the smallest bound in force; a tie keeps the per-run cap, which
// is no cap stop. It locks the cell and the parent sink, so a caller must
// not hold a sink's mu.
func (l runLimit) read() capRead {
	var r capRead

	if l.perRunUSD > 0 {
		r.usd, r.capped = l.perRunUSD, true
	}

	if l.blueprint != nil {
		if r.blueprintUSD = l.blueprint.get(); r.blueprintUSD > 0 {
			if left := r.blueprintUSD - l.baseUSD; !r.capped || left < r.usd {
				r.usd, r.capped, r.bound = left, true, true
			}
		}
	}

	if left, ok := l.parent.left(); ok && (!r.capped || left < r.usd) {
		r.usd, r.capped, r.bound = left, true, false
	}

	return r
}

// fixedUSD is the harness's own cap. The harness copies its config at
// start, so a priced run, whose sink binds the live limit, gives it only the
// per-run cap, which no reply moves; an unpriced run has no sink to bind it
// and gets the limit as it stands.
func (l runLimit) fixedUSD(priced bool) float64 {
	if priced {
		return l.perRunUSD
	}

	return l.read().usd
}

// capOf is blueprint id's live cap, created on first use.
func (c *Controller) capOf(id string) *liveCap {
	c.mu.Lock()
	defer c.mu.Unlock()

	l := c.caps[id]
	if l == nil {
		l = &liveCap{}
		c.caps[id] = l
	}

	return l
}
