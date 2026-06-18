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

// UserOrdersTopic is the per-user WebSocket topic for order state changes.
func UserOrdersTopic(userID string) string {
	return "user:" + strings.TrimSpace(userID) + ":orders"
}

// UserPositionsTopic is the per-user WebSocket topic for position changes.
func UserPositionsTopic(userID string) string {
	return "user:" + strings.TrimSpace(userID) + ":positions"
}
