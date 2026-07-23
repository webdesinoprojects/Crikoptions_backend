package simulator

import (
	"context"
	"errors"
	"log"
	"time"
)

// FallbackGate is the matches-service surface the fallback controller needs to
// decide when the demo replays should be visible.
type FallbackGate interface {
	// ProviderMatchImminent reports whether a real provider match is in play or
	// is scheduled to start within the given lead time.
	ProviderMatchImminent(ctx context.Context, within time.Duration) (bool, error)
	// SetDemoMatchesHidden reveals (hidden=false) or hides (hidden=true) the demo
	// match documents so they enter or leave the home feed.
	SetDemoMatchesHidden(ctx context.Context, hidden bool, matchIDs ...string) error
}

// FallbackSpecs returns the built-in replays used as a no-live-match fallback:
// CSK vs MI and RCB vs KKR. They loop while active so the terminal always has a
// tradable game between real fixtures.
func FallbackSpecs() []AutoStartSpec {
	return []AutoStartSpec{
		{MatchID: "0000000000000000000000aa", ScriptName: "csk_vs_mi"},
		{MatchID: "0000000000000000000000bb", ScriptName: "rcb_vs_kkr"},
	}
}

// FallbackController surfaces the built-in CSV replays as tradable demo matches
// whenever no real provider match is in play, and hides/stops them again the
// moment a real match goes live. It is safe to run on every API instance: replay
// ownership is arbitrated by the simulator's distributed lease.
type FallbackController struct {
	sim      *Service
	gate     FallbackGate
	specs    []AutoStartSpec
	interval time.Duration
	lead     time.Duration
	active   bool
}

// NewFallbackController builds a controller for the given specs. A zero interval
// defaults to 20s; a zero lead defaults to 30m (how far before a real match the
// demo games are wound down and their trades squared off).
func NewFallbackController(sim *Service, gate FallbackGate, specs []AutoStartSpec, interval, lead time.Duration) *FallbackController {
	if interval <= 0 {
		interval = 20 * time.Second
	}
	if lead <= 0 {
		lead = 30 * time.Minute
	}
	return &FallbackController{sim: sim, gate: gate, specs: specs, interval: interval, lead: lead}
}

// Run reconciles immediately and then on a ticker until ctx is cancelled.
func (c *FallbackController) Run(ctx context.Context) {
	log.Printf("simulator fallback: controller started (interval=%s lead=%s specs=%d)", c.interval, c.lead, len(c.specs))
	c.reconcile(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

func (c *FallbackController) reconcile(ctx context.Context) {
	imminent, err := c.gate.ProviderMatchImminent(ctx, c.lead)
	if err != nil {
		log.Printf("simulator fallback: check imminent real match: %v", err)
		return
	}
	if imminent {
		if c.active {
			c.deactivate(ctx)
		}
		return
	}
	c.ensureActive(ctx)
}

// ensureActive reveals the demo matches and (re)starts any replay that is not
// running locally. It is idempotent and restart-safe: it always uses a fresh
// Start (which steals expired leases and resets a completed/hidden demo match
// back to live), so a hard-killed previous instance never leaves the fallback
// stuck. A replay owned by another live instance (ErrLockHeld) is left alone.
func (c *FallbackController) ensureActive(ctx context.Context) {
	if !c.active {
		if err := c.gate.SetDemoMatchesHidden(ctx, false, c.matchIDs()...); err != nil {
			log.Printf("simulator fallback: reveal demo matches: %v", err)
			return
		}
		c.active = true
	}
	for _, spec := range c.specs {
		switch c.sim.Status(spec.MatchID).Status {
		case StatusRunning, StatusPaused:
			continue // already replaying on this instance.
		}
		if _, err := c.sim.Start(ctx, spec.MatchID, StartRequest{ScriptName: spec.ScriptName}); err != nil {
			if errors.Is(err, ErrLockHeld) {
				continue // owned by another (or a not-yet-expired) lease; retry next tick.
			}
			log.Printf("simulator fallback: start %s (%s): %v", spec.MatchID, spec.ScriptName, err)
			continue
		}
		log.Printf("simulator fallback: activated demo match %s (%s)", spec.MatchID, spec.ScriptName)
	}
}

func (c *FallbackController) deactivate(ctx context.Context) {
	// Stop the replays first so no new balls (and no new tradable prices) arrive
	// while we exit positions.
	for _, spec := range c.specs {
		c.sim.StopMatch(spec.MatchID)
	}
	// HARD RULE: exit all open trades on the DEMO matches ONLY. SquareOffMatch is
	// scoped strictly by match id, so a real match's positions are never touched.
	c.squareOffDemoMatches(ctx)
	if err := c.gate.SetDemoMatchesHidden(ctx, true, c.matchIDs()...); err != nil {
		log.Printf("simulator fallback: hide demo matches: %v", err)
	}
	log.Printf("simulator fallback: real match imminent (within %s) — squared off demo trades and hid demo games", c.lead)
	c.active = false
}

// squareOffDemoMatches exits every open position on the fallback demo matches.
// It is strictly limited to the demo match ids and is a safe no-op when a match
// has no open positions, so it can run on every deactivation.
func (c *FallbackController) squareOffDemoMatches(ctx context.Context) {
	if c.sim.squareOff == nil {
		return
	}
	for _, spec := range c.specs {
		if err := c.sim.squareOff.SquareOffMatch(ctx, spec.MatchID); err != nil {
			log.Printf("simulator fallback: square off demo match %s: %v", spec.MatchID, err)
			continue
		}
		log.Printf("simulator fallback: exited all demo trades for %s (%s)", spec.MatchID, spec.ScriptName)
	}
}

func (c *FallbackController) matchIDs() []string {
	ids := make([]string, len(c.specs))
	for i, spec := range c.specs {
		ids[i] = spec.MatchID
	}
	return ids
}
