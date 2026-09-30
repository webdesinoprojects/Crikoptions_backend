package cricketline

import (
	"math"
	"testing"
)

func TestEncodeKeyRoundTrip(t *testing.T) {
	seen := map[int64]string{}
	for _, key := range []string{"0", "A", "0A", "A0", "14F7", "JG", "6E", "2MT", "1CX", "VSV", "zzzzzzzz", "ZZ"} {
		id, err := EncodeKey(key)
		if err != nil {
			t.Fatalf("EncodeKey(%q): %v", key, err)
		}
		if !IsFeedID(id) || id >= 1<<53 || id <= IDNamespace {
			t.Fatalf("EncodeKey(%q) = %d outside the feed range or JSON-safe integers", key, id)
		}
		if previous, clash := seen[id]; clash {
			t.Fatalf("%q and %q share id %d", key, previous, id)
		}
		seen[id] = key
		back, err := DecodeKey(id)
		if err != nil || back != key {
			t.Fatalf("DecodeKey(%d) = %q, %v; want %q", id, back, err, key)
		}
	}
	if IDMax > 1<<53 {
		t.Fatalf("IDMax %d exceeds 2^53", IDMax)
	}
	for _, bad := range []string{"", "14-F7", "123456789", "é"} {
		if _, err := EncodeKey(bad); err == nil {
			t.Fatalf("EncodeKey(%q) accepted an invalid key", bad)
		}
	}
	if _, err := DecodeKey(123456); err == nil {
		t.Fatal("a CricLive id decoded as a CricketLine key")
	}
}

func TestNormalizeTokens(t *testing.T) {
	tests := map[string]string{
		"0": "0", "4": "4", "6": "6", "W": "W", "w": "W", "1w": "1W",
		"wd": "Wd1", "WD": "Wd1", "nb": "N0", "4nb": "N4", "1lb": "L1", "lb": "L1",
		"2b": "B2", "b": "B1", "nbw": "N0W", "2wd": "Wd3", "wd2": "Wd3",
	}
	for raw, want := range tests {
		ball, ok := parseCrexToken(raw)
		if !ok {
			t.Fatalf("parseCrexToken(%q) failed", raw)
		}
		if got := ball.cricLiveToken(false); got != want {
			t.Fatalf("%q -> %q, want %q", raw, got, want)
		}
	}
	for _, bad := range []string{"", "x", "4xyz"} {
		if _, ok := parseCrexToken(bad); ok {
			t.Fatalf("parseCrexToken(%q) accepted junk", bad)
		}
	}
}

func TestNormalizeOverCalibratesNumberedExtras(t *testing.T) {
	// Read as "runs on top of the wide", 2wd is three runs: the over is 7.
	tokens, _ := normalizeOver([]string{"1", "2wd", "4"}, 8, true)
	if overTotal(tokens) != 8 || tokens[1] != "Wd3" {
		t.Fatalf("tokens = %v (total %d)", tokens, overTotal(tokens))
	}
	// The feed says the over was 7: then 2wd was two in all.
	tokens, _ = normalizeOver([]string{"1", "2wd", "4"}, 7, true)
	if overTotal(tokens) != 7 || tokens[1] != "Wd2" {
		t.Fatalf("tokens = %v (total %d), want the total reading", tokens, overTotal(tokens))
	}
}

func TestAssignInnings(t *testing.T) {
	tests := []struct {
		name          string
		numbers       []int
		current       int
		legal         int
		previousFinal int
		want          []int
	}{
		{"one innings", []int{16, 17, 18, 19}, 2, 113, 20, []int{2, 2, 2, 2}},
		{"straddles the change", []int{18, 19, 20, 1}, 2, 3, 20, []int{1, 1, 1, 2}},
		{"second innings not started", []int{18, 19, 20}, 2, 0, 20, []int{1, 1, 1}},
		{"first ball ahead of the scoreboard", []int{1}, 2, 0, 20, []int{2}},
		{"first innings", []int{1, 2, 3}, 1, 15, 0, []int{1, 1, 1}},
		{"all out early", []int{16, 17, 18, 19}, 2, 0, 19, []int{1, 1, 1, 1}},
	}
	for _, test := range tests {
		got := assignInnings(test.numbers, test.current, test.legal, test.previousFinal)
		for i := range test.want {
			if got[i] != test.want[i] {
				t.Fatalf("%s: got %v, want %v", test.name, got, test.want)
			}
		}
	}
}

func TestParseSideScore(t *testing.T) {
	tests := []struct {
		score, overs string
		want         sideScore
	}{
		{"148-4", "18.5", sideScore{148, 4, 18.5, true, false}},
		{"160/5", "(20.0)", sideScore{160, 5, 20, true, false}},
		{"333", "49.5", sideScore{333, 10, 49.5, true, true}},
		{"0-0", "0.0", sideScore{0, 0, 0, true, false}},
		{"", "Yet To Bat", sideScore{}},
	}
	for _, test := range tests {
		got := parseSideScore(test.score, test.overs)
		if got.runs != test.want.runs || got.wickets != test.want.wickets ||
			math.Abs(got.overs-test.want.overs) > 1e-9 || got.present != test.want.present || got.bare != test.want.bare {
			t.Fatalf("parseSideScore(%q, %q) = %+v, want %+v", test.score, test.overs, got, test.want)
		}
	}
}
