package executions

import "testing"

func TestParseLegalBalls(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"0.1", 1},
		{"6.0", 36},
		{"15.5", 95},
		{"19.1", 115},
		{"5.4", 34},
		{"15.0", 90},
		{"15.1", 91},
		{"10.1", 61},
		{"40.1", 241},
		{"49.2", 296},
		{"", 0},
		{"0.0", 0},
	}
	for _, tc := range cases {
		if got := ParseLegalBalls(tc.in); got != tc.want {
			t.Fatalf("ParseLegalBalls(%q)=%d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveLegalBallsPrefersStored(t *testing.T) {
	got := EffectiveLegalBalls(MatchClock{OversText: "5.4", LegalBalls: 34})
	if got != 34 {
		t.Fatalf("got %d", got)
	}
	got = EffectiveLegalBalls(MatchClock{OversText: "5.4"})
	if got != 34 {
		t.Fatalf("fallback parse got %d", got)
	}
}

func TestNormalizeChallengeFormat(t *testing.T) {
	if got := NormalizeChallengeFormat(""); got != "T20" {
		t.Fatalf("missing format %q, want T20", got)
	}
	if got := NormalizeChallengeFormat("T20I"); got != "T20" {
		t.Fatalf("T20I %q, want T20", got)
	}
	if got := NormalizeChallengeFormat("ODI"); got != "ODI" {
		t.Fatalf("ODI %q", got)
	}
	if got := NormalizeChallengeFormat("T10"); got != "" {
		t.Fatalf("T10 should be ignored, got %q", got)
	}
}
