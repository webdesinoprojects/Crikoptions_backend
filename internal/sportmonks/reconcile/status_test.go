package reconcile

import "testing"

func TestIsExplicitTerminalProviderStatus(t *testing.T) {
	cases := map[string]bool{
		"Finished":    true,
		"Aban.":       true,
		"Cancl.":      true,
		"1st Innings": false,
		"NS":          false,
		"LIVE":        false,
	}
	for status, want := range cases {
		if got := IsExplicitTerminalProviderStatus(status); got != want {
			t.Fatalf("status %q: got %v want %v", status, got, want)
		}
	}
}

func TestFixtureProviderStatus(t *testing.T) {
	raw := []byte(`{"id":69905,"status":"Finished","localteam_id":1,"visitorteam_id":2}`)
	status, ok := FixtureProviderStatus(raw)
	if !ok || status != "Finished" {
		t.Fatalf("status=%q ok=%v", status, ok)
	}
}

func TestNormalizeProviderStatusFinished(t *testing.T) {
	if got := NormalizeProviderStatus("Finished"); got != "completed" {
		t.Fatalf("got %q", got)
	}
}
