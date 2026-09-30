package cricketline

import (
	"math"
	"strconv"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
)

// Ball tokens as CricketLineApi prints them, verified against live overs:
// "0".."6" runs off the bat, "W" a wicket, "wd" a one-run wide, "nb" a no-ball
// with nothing off the bat, "1lb" one leg bye. A leading number carries the
// runs on extras ("2wd", "4nb", "2b").
//
// The reducer prices CricLive's notation (reconcile.ParseBallToken), so each
// token is rewritten into it rather than teaching the reducer a second
// grammar. "2wd" and "4nb" are ambiguous — runs on top of the extra, or the
// total — so each over is checked against the provider's own over total and
// the reading that reproduces it wins.

type crexBall struct {
	runs    int
	hasRuns bool
	kind    string // "", "wd", "nb", "lb", "b", "p"
	wicket  bool
}

func parseCrexToken(raw string) (crexBall, bool) {
	token := strings.ToLower(strings.Join(strings.Fields(raw), ""))
	if token == "" {
		return crexBall{}, false
	}
	var ball crexBall
	if token == "w" {
		return crexBall{wicket: true}, true
	}
	// A wicket marker sits at either end ("1w", "w1", "nbw"); "wd" is a wide.
	if strings.HasPrefix(token, "w") && !strings.HasPrefix(token, "wd") {
		ball.wicket = true
		token = token[1:]
	} else if strings.HasSuffix(token, "w") {
		ball.wicket = true
		token = token[:len(token)-1]
	}
	digits := 0
	for digits < len(token) && token[digits] >= '0' && token[digits] <= '9' {
		digits++
	}
	if digits > 0 {
		ball.runs, _ = strconv.Atoi(token[:digits])
		ball.hasRuns = true
	}
	rest := token[digits:]
	// Tolerate the number after the extra as well ("wd2", "lb1").
	if trailing := strings.TrimLeft(rest, "abcdefghijklmnopqrstuvwxyz"); trailing != "" && !ball.hasRuns {
		if value, err := strconv.Atoi(trailing); err == nil {
			ball.runs, ball.hasRuns = value, true
			rest = strings.TrimSuffix(rest, trailing)
		}
	}
	switch rest {
	case "":
		if !ball.hasRuns && !ball.wicket {
			return crexBall{}, false
		}
	case "wd", "wide", "wides":
		ball.kind = "wd"
	case "nb", "noball":
		ball.kind = "nb"
	case "lb", "legbye", "legbyes":
		ball.kind = "lb"
	case "b", "bye", "byes":
		ball.kind = "b"
	case "p", "pen", "penalty":
		ball.kind = "p"
	default:
		return crexBall{}, false
	}
	return ball, true
}

// cricLiveToken renders the ball in CricLive notation. alt selects the
// "number is the total" reading of a numbered wide or no-ball.
func (b crexBall) cricLiveToken(alt bool) string {
	wicket := ""
	if b.wicket {
		wicket = "W"
	}
	count := func(fallback int) int {
		if b.hasRuns {
			return b.runs
		}
		return fallback
	}
	switch b.kind {
	case "wd":
		total := count(0) + 1
		if alt && b.hasRuns && b.runs > 0 {
			total = b.runs
		}
		// CricLive notation cannot carry a wicket on a wide (a stumping off
		// one); the scoreboard, not the ball strip, owns the wicket count.
		return "Wd" + strconv.Itoa(total)
	case "nb":
		bat := count(0)
		if alt && b.hasRuns && b.runs > 0 {
			bat = b.runs - 1
		}
		return "N" + strconv.Itoa(bat) + wicket
	case "lb":
		return "L" + strconv.Itoa(count(1)) + wicket
	case "b":
		return "B" + strconv.Itoa(count(1)) + wicket
	case "p":
		return "P" + strconv.Itoa(count(5))
	}
	if !b.hasRuns {
		return "W"
	}
	return strconv.Itoa(b.runs) + wicket
}

func (b crexBall) ambiguous() bool {
	return (b.kind == "wd" || b.kind == "nb") && b.hasRuns && b.runs > 0
}

// normalizeOver rewrites one over's tokens. overRuns is the provider's total
// for the over; when known it picks between the two readings of numbered
// extras. Tokens that cannot be read are passed through unchanged, which the
// reducer then skips as display-only.
func normalizeOver(tokens []string, overRuns int, haveRuns bool) (out []string, unreadable int) {
	balls := make([]crexBall, len(tokens))
	readable := make([]bool, len(tokens))
	anyAmbiguous := false
	for i, token := range tokens {
		balls[i], readable[i] = parseCrexToken(token)
		if !readable[i] {
			unreadable++
		}
		anyAmbiguous = anyAmbiguous || (readable[i] && balls[i].ambiguous())
	}
	render := func(alt bool) []string {
		rendered := make([]string, len(tokens))
		for i := range tokens {
			if readable[i] {
				rendered[i] = balls[i].cricLiveToken(alt)
			} else {
				rendered[i] = strings.TrimSpace(tokens[i])
			}
		}
		return rendered
	}
	primary := render(false)
	if !anyAmbiguous || !haveRuns || overTotal(primary) == overRuns {
		return primary, unreadable
	}
	if alternative := render(true); overTotal(alternative) == overRuns {
		return alternative, unreadable
	}
	return primary, unreadable
}

func overTotal(tokens []string) int {
	total := 0
	for _, token := range tokens {
		if outcome, err := reconcile.ParseBallToken(token); err == nil {
			total += outcome.TotalRuns
		}
	}
	return total
}

// overNumber reads "Over 19" (the nineteenth over, 18.1–18.6) as 19.
func overNumber(label string) int {
	fields := strings.Fields(label)
	for i := len(fields) - 1; i >= 0; i-- {
		if value, err := strconv.Atoi(fields[i]); err == nil && value > 0 {
			return value
		}
	}
	return 0
}

// assignInnings decides which innings each over of a recent-overs window
// belongs to. The window is oldest first and can straddle an innings change.
// Walking back from the newest over, over numbers fall strictly within one
// innings, so a number that does not fall marks the previous innings.
//
// With no such break the whole window is one innings: the current one, except
// when the current innings has not started (no legal ball yet) and the
// window's newest over is the previous innings' last — then it is all the
// previous innings. Matching on that last over, not merely on "no balls yet",
// keeps a first ball the scoreboard has not caught up with from being filed
// under an innings that has already closed.
func assignInnings(numbers []int, current, currentLegalBalls, previousFinalOver int) []int {
	out := make([]int, len(numbers))
	innings := current
	newest := -1
	for i := len(numbers) - 1; i >= 0; i-- {
		if numbers[i] > 0 {
			newest = i
			break
		}
	}
	if newest < 0 {
		return out
	}
	hasBreak := false
	last := math.MaxInt
	for i := newest; i >= 0; i-- {
		if numbers[i] <= 0 {
			continue
		}
		if numbers[i] == last {
			// The same over listed twice cannot be placed with confidence, and
			// misplacing it could rewrite a settled innings: skip the strip for
			// this read. The scoreboard still updates.
			return make([]int, len(numbers))
		}
		if numbers[i] > last {
			hasBreak = true
		}
		last = numbers[i]
	}
	switch {
	case hasBreak && current == 1:
		// The strip has moved on to an innings the scoreboard has not: the
		// newest overs belong to innings 2 and are dropped (below) until the
		// scoreboard catches up, rather than filed under innings 1.
		innings = 2
	case !hasBreak && current > 1 && currentLegalBalls == 0 && numbers[newest] == previousFinalOver:
		innings = current - 1
	}
	last = math.MaxInt
	for i := newest; i >= 0; i-- {
		if numbers[i] <= 0 {
			continue
		}
		if numbers[i] > last {
			innings--
		}
		last = numbers[i]
		if innings < 1 {
			break
		}
		if innings <= current {
			out[i] = innings
		}
	}
	return out
}
