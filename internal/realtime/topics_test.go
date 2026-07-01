package realtime

import "testing"

func TestMatchTopicsUseHexID(t *testing.T) {
	hexBB := "0000000000000000000000bb"
	wantScore := "match:score:" + hexBB
	wantCommentary := "match:commentary:" + hexBB

	if got := MatchScoreTopic(hexBB); got != wantScore {
		t.Fatalf("MatchScoreTopic = %q, want %q", got, wantScore)
	}
	if got := MatchCommentaryTopic(hexBB); got != wantCommentary {
		t.Fatalf("MatchCommentaryTopic = %q, want %q", got, wantCommentary)
	}
}
