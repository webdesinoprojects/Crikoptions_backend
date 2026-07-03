package simulator

import "testing"

func TestResolveIntervalSec_EnvOverridesCSV(t *testing.T) {
	svc := &Service{cfg: Config{DefaultIntervalSec: 4}}
	if got := svc.resolveIntervalSec(0, 15); got != 4 {
		t.Fatalf("resolveIntervalSec = %d, want 4", got)
	}
}

func TestResolveIntervalSec_RequestWins(t *testing.T) {
	svc := &Service{cfg: Config{DefaultIntervalSec: 4}}
	if got := svc.resolveIntervalSec(8, 15); got != 8 {
		t.Fatalf("resolveIntervalSec = %d, want 8", got)
	}
}

func TestDeliveryDelay_UsesWorkerInterval(t *testing.T) {
	w := &Worker{intervalSec: 4}
	if got := w.deliveryDelay(15); got.Seconds() != 4 {
		t.Fatalf("deliveryDelay = %v, want 4s", got)
	}
}

func TestDeliveryDelay_FallsBackToCSV(t *testing.T) {
	w := &Worker{intervalSec: 0}
	if got := w.deliveryDelay(15); got.Seconds() != 15 {
		t.Fatalf("deliveryDelay = %v, want 15s", got)
	}
}
