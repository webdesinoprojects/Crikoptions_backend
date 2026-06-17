package realtime

import "strings"

// MatchScoreTopic is the WebSocket topic for live score/overs updates.
func MatchScoreTopic(matchID string) string {
	return "match:score:" + normalizeMatchID(matchID)
}

// MatchCommentaryTopic is the WebSocket topic for ball-by-ball "This over" UI.
func MatchCommentaryTopic(matchID string) string {
	return "match:commentary:" + normalizeMatchID(matchID)
}

func normalizeMatchID(matchID string) string {
	return strings.TrimSpace(matchID)
}
