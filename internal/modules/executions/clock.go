package executions

import (
	"strconv"
	"strings"
	"time"
)

// MatchClock is the live innings clock copied onto a fill at execution time.
// Progress for daily challenges must use this snapshot, never a later poll.
type MatchClock struct {
	OversText  string    `json:"oversText,omitempty" bson:"oversText,omitempty"`
	LegalBalls int       `json:"legalBalls,omitempty" bson:"legalBalls,omitempty"`
	Innings    int       `json:"innings,omitempty" bson:"innings,omitempty"`
	Format     string    `json:"format,omitempty" bson:"format,omitempty"`
	MatchID    string    `json:"matchId,omitempty" bson:"matchId,omitempty"`
	At         time.Time `json:"at,omitempty" bson:"at,omitempty"`
}

func (c MatchClock) IsZero() bool {
	return c.OversText == "" && c.LegalBalls == 0 && c.Innings == 0 && c.Format == "" && c.MatchID == "" && c.At.IsZero()
}

// ParseLegalBalls converts oversText "major.minor" into legal balls bowled.
// minor is balls in the current over (0–5). "6.0" → 36, "19.1" → 115.
func ParseLegalBalls(oversText string) int {
	s := strings.TrimSpace(oversText)
	if s == "" {
		return 0
	}
	major, minor := 0, 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		major, _ = strconv.Atoi(strings.TrimSpace(s[:i]))
		minor, _ = strconv.Atoi(strings.TrimSpace(s[i+1:]))
	} else {
		major, _ = strconv.Atoi(s)
	}
	if major < 0 {
		major = 0
	}
	if minor < 0 {
		minor = 0
	}
	return major*6 + minor
}

// EffectiveLegalBalls prefers a stored legal-ball count when it is present.
func EffectiveLegalBalls(c MatchClock) int {
	if c.LegalBalls > 0 {
		return c.LegalBalls
	}
	return ParseLegalBalls(c.OversText)
}

// NormalizeChallengeFormat maps a match format onto T20, ODI, or empty
// (unsupported). A missing format is treated as T20. T10 and other formats
// are empty so daily over-window challenges ignore the fill.
func NormalizeChallengeFormat(format string) string {
	u := strings.ToUpper(strings.TrimSpace(format))
	if u == "" {
		return "T20"
	}
	if strings.Contains(u, "T10") {
		return ""
	}
	if strings.Contains(u, "ODI") || strings.Contains(u, "ONE DAY") || strings.Contains(u, "ONE-DAY") {
		return "ODI"
	}
	if strings.Contains(u, "T20") || strings.Contains(u, "TWENTY20") || strings.Contains(u, "TWENTY 20") {
		return "T20"
	}
	return ""
}
