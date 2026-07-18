package store

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
)

func TestApplyTerminalMatchState(t *testing.T) {
	match := &matches.Match{Status: matches.StatusLive, FeedState: matches.FeedStateReconciling}
	if !applyTerminalMatchState(match, "Finished") {
		t.Fatal("expected terminal apply")
	}
	if match.Status != matches.StatusCompleted || match.FeedState != matches.FeedStateTerminal {
		t.Fatalf("state=%s/%s", match.Status, match.FeedState)
	}
}

func TestProjectMatchImmediateTerminalWhenProviderFinished(t *testing.T) {
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusCompleted, "final")
	projection.ProviderStatus = "Finished"
	current := initialMatch(projection, start)
	next := projectMatch(current, projection, start, time.Minute, 2*time.Minute, 50*time.Second, 1)
	if next.Status != matches.StatusCompleted || next.FeedState != matches.FeedStateTerminal {
		t.Fatalf("state=%s/%s", next.Status, next.FeedState)
	}
	if !reconcile.IsExplicitTerminalProviderStatus(projection.ProviderStatus) {
		t.Fatal("expected explicit terminal provider status")
	}
}
