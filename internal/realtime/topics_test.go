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

func TestProtectedTopicAuthorization(t *testing.T) {
	handler := NewHandler(NewHub(), nil)
	handler.SetChatEnabled(true)
	client := &Client{topics: make(map[string]struct{})}

	if handler.canSubscribe(client, "chat:room:global") {
		t.Fatal("anonymous client subscribed to chat")
	}
	client.setUserID("user-1")
	if !handler.canSubscribe(client, "chat:room:global") {
		t.Fatal("authenticated client could not subscribe to chat")
	}
	if !handler.canSubscribe(client, "user:user-1:orders") {
		t.Fatal("user could not subscribe to own topic")
	}
	if handler.canSubscribe(client, "user:user-2:orders") {
		t.Fatal("user subscribed to another user's topic")
	}
	if handler.canSubscribe(client, "unknown:topic") {
		t.Fatal("unknown topic was allowed")
	}
}
