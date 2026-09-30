package cricketline

import (
	"strings"
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
)

// CricketLineApi lists the two batters at the crease without saying which is
// on strike (/api/live has an on_strike flag, but it was seen to lag a ball).
// Balls faced settle it: between two reads of the same innings, every ball a
// batter faced is a delivery in the over strip, and strike then rotates by the
// laws — odd runs run, the end of an over, a new batter in for a dismissed one.
// Replaying the deliveries in between from each candidate striker and keeping
// the one whose balls-faced counts match the scoreboard identifies the striker
// without guessing. A read that cannot be reconciled (two wickets between
// reads, a gap wider than the window, a scoreboard lagging its ball strip)
// leaves the last confirmed state in place and is retried from it.

// strikeMemoTTL bounds how long an unreconcilable read may keep the tracker on
// its last confirmed state before it starts again from the current one.
const strikeMemoTTL = 3 * time.Minute

// strikeMaxMisses is how many consecutive unreconcilable reads the tracker
// waits out before starting again from the current read.
const strikeMaxMisses = 2

type crease struct {
	name  string
	runs  int
	balls int
}

type ballKey struct {
	over  int
	index int
}

func (k ballKey) less(other ballKey) bool {
	return k.over < other.over || (k.over == other.over && k.index < other.index)
}

// trackedBall is one delivery of the current innings, oldest first, in
// CricLive notation, with whether it completed its over.
type trackedBall struct {
	key     ballKey
	token   string
	overEnd bool
}

type strikeMemo struct {
	innings   int
	lastKey   ballKey
	hasBalls  bool
	batters   []crease
	striker   string // "" while unconfirmed
	confirmed time.Time
	misses    int
}

type strikeHint struct {
	batters  []crease
	striker  string
	observed time.Time
}

type strikeTracker struct {
	mu    sync.Mutex
	memos map[string]*strikeMemo
	hints map[string]strikeHint
}

func newStrikeTracker() *strikeTracker {
	return &strikeTracker{memos: map[string]*strikeMemo{}, hints: map[string]strikeHint{}}
}

// hint records /api/live's on_strike reading, used only to seed a match the
// tracker has never confirmed.
func (t *strikeTracker) hint(match string, batters []crease, striker string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hints[match] = strikeHint{batters: batters, striker: striker, observed: now}
}

// resolve returns the striker for this read of a match.
func (t *strikeTracker) resolve(match string, innings int, balls []trackedBall, batters []crease, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := make([]crease, 0, 2)
	for _, batter := range batters {
		if strings.TrimSpace(batter.name) != "" {
			current = append(current, batter)
		}
	}
	if len(current) == 0 {
		return ""
	}
	if len(current) == 1 || innings <= 0 {
		delete(t.memos, match)
		return current[0].name
	}

	memo := t.memos[match]
	if memo != nil && memo.innings == innings {
		if since, ok := ballsSince(balls, memo); ok {
			if striker := replayStrike(memo, since, current); striker != "" {
				t.memos[match] = newMemo(innings, balls, current, striker, now)
				return striker
			}
		}
		// Unreconciled: keep the confirmed state while it is recent enough to
		// replay from; a scoreboard lagging its ball strip catches up on the
		// next read. A second miss means the gap cannot be replayed (two
		// wickets, a window that moved on), so start again from this read.
		memo.misses++
		if memo.misses < strikeMaxMisses && now.Sub(memo.confirmed) < strikeMemoTTL {
			if memo.striker != "" && hasBatter(current, memo.striker) {
				return memo.striker
			}
			return current[0].name
		}
	}

	striker := ""
	if hint, ok := t.hints[match]; ok && sameCrease(hint.batters, current) && hasBatter(current, hint.striker) {
		striker = hint.striker
	}
	t.memos[match] = newMemo(innings, balls, current, striker, now)
	if striker == "" {
		return current[0].name
	}
	return striker
}

func (t *strikeTracker) forget(match string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.memos, match)
	delete(t.hints, match)
}

func newMemo(innings int, balls []trackedBall, batters []crease, striker string, now time.Time) *strikeMemo {
	memo := &strikeMemo{innings: innings, batters: batters, striker: striker, confirmed: now}
	if len(balls) > 0 {
		memo.lastKey, memo.hasBalls = balls[len(balls)-1].key, true
	}
	return memo
}

// ballsSince returns the deliveries after the memo's last one. It fails when
// the window no longer reaches back to it, or the provider withdrew it.
func ballsSince(balls []trackedBall, memo *strikeMemo) ([]trackedBall, bool) {
	if !memo.hasBalls {
		return balls, true
	}
	for i, ball := range balls {
		if ball.key == memo.lastKey {
			return balls[i+1:], true
		}
	}
	return nil, false
}

// replayStrike returns the only striker consistent with the balls faced since
// the memo, or "" when none or both are.
func replayStrike(memo *strikeMemo, since []trackedBall, current []crease) string {
	if len(memo.batters) != 2 {
		return ""
	}
	candidates := []string{memo.batters[0].name, memo.batters[1].name}
	if memo.striker != "" {
		candidates = []string{memo.striker}
	}
	found := ""
	for _, candidate := range candidates {
		striker, ok := simulateStrike(memo.batters, candidate, since, current)
		if !ok {
			continue
		}
		if found != "" && found != striker {
			return ""
		}
		found = striker
	}
	return found
}

func simulateStrike(previous []crease, first string, since []trackedBall, current []crease) (string, bool) {
	on, off := first, otherBatter(previous, first)
	dismissed, newcomer := "", ""
	for _, batter := range previous {
		if !hasBatter(current, batter.name) {
			if dismissed != "" {
				return "", false
			}
			dismissed = batter.name
		}
	}
	for _, batter := range current {
		if !hasBatter(previous, batter.name) {
			if newcomer != "" {
				return "", false
			}
			newcomer = batter.name
		}
	}
	replace := func() bool {
		switch {
		case dismissed == "" || newcomer == "":
			return true
		case on == dismissed:
			on = newcomer
		case off == dismissed:
			off = newcomer
		default:
			return false
		}
		dismissed = ""
		return true
	}

	wicketSince := false
	for _, ball := range since {
		if outcome, err := reconcile.ParseBallToken(ball.token); err == nil && outcome.IsWicket {
			wicketSince = true
		}
	}
	// The wicket fell before these balls (in an earlier read) and the new
	// batter has only now appeared: they came in at the dismissed batter's end
	// before any of them, and may already have faced some.
	if !wicketSince {
		if (dismissed != "") != (newcomer != "") || !replace() {
			return "", false
		}
	}

	faced := map[string]int{}
	wickets := 0
	for _, ball := range since {
		outcome, err := reconcile.ParseBallToken(ball.token)
		if err != nil {
			return "", false
		}
		if outcome.Extras.Wides == 0 {
			faced[on]++
		}
		if outcome.IsWicket {
			wickets++
			if wickets > 1 || !replace() {
				return "", false
			}
		} else if runsRun(outcome)%2 == 1 {
			on, off = off, on
		}
		if ball.overEnd {
			on, off = off, on
		}
	}
	for _, batter := range current {
		before := 0
		for _, earlier := range previous {
			if earlier.name == batter.name {
				before = earlier.balls
			}
		}
		if faced[batter.name] != batter.balls-before {
			return "", false
		}
	}
	return on, true
}

// runsRun is how many runs the batters physically ran, which is what moves
// strike: runs off the bat, byes and leg byes, and anything beyond the wide's
// own penalty run.
func runsRun(outcome reconcile.BallOutcome) int {
	switch {
	case outcome.Extras.Wides > 0:
		return outcome.TotalRuns - 1
	case outcome.Extras.NoBalls > 0:
		return outcome.BatterRuns
	case outcome.Extras.Penalties > 0:
		return 0
	}
	return outcome.TotalRuns
}

func otherBatter(batters []crease, name string) string {
	for _, batter := range batters {
		if batter.name != name {
			return batter.name
		}
	}
	return ""
}

func hasBatter(batters []crease, name string) bool {
	for _, batter := range batters {
		if batter.name == name {
			return true
		}
	}
	return false
}

func sameCrease(a, b []crease) bool {
	if len(a) != len(b) {
		return false
	}
	for _, left := range a {
		match := false
		for _, right := range b {
			if left == right {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}
