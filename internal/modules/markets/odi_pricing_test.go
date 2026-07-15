package markets

import (
	"testing"
)

func TestODIPricingEngine(t *testing.T) {
	cfg := DefaultODIPricingConfig()

	t.Run("config matches HTML calculator", func(t *testing.T) {
		if cfg.BallsTotal != 300 {
			t.Fatalf("BallsTotal = %d, want 300", cfg.BallsTotal)
		}
		if cfg.MaxStrike != 350 || cfg.StrikeStep != 10 {
			t.Fatalf("strikes = %.0f step %.0f, want 350 / 10", cfg.MaxStrike, cfg.StrikeStep)
		}
		if !cfg.TrueZero {
			t.Fatal("TrueZero should be enabled for ODI")
		}
		if cfg.MaxProjectedS0 != 440 || cfg.Sigma1 != 0.28 || cfg.R1 != 0.18 {
			t.Fatalf("1st-innings params S0cap/sigma/r = %.0f/%.2f/%.2f", cfg.MaxProjectedS0, cfg.Sigma1, cfg.R1)
		}
	})

	t.Run("1st innings start projects with default RR", func(t *testing.T) {
		r := CalculateFirstInnings(PricingInput{
			Innings: 1, Score: 0, Wickets: 0, BallsLeft: 299,
		}, cfg)
		if len(r.Chain) != 35 {
			t.Fatalf("expected 35 strikes (10..350), got %d", len(r.Chain))
		}
		// S0 ≈ 0 + min(299*1.22, 440/(1+0.8*299/300))
		if r.S0 < 200 || r.S0 > 280 {
			t.Fatalf("S0 = %.2f, want roughly 240–260 from HTML defaults", r.S0)
		}
		if r.EffectiveRunRate != 1.22 {
			t.Fatalf("RR = %.2f, want 1.22", r.EffectiveRunRate)
		}
		if r.Chain[0].Premium <= 0 {
			t.Errorf("strike 10 should have positive premium, got %.2f", r.Chain[0].Premium)
		}
		// Deep OTM should be near true-zero (not floored up).
		last := r.Chain[len(r.Chain)-1]
		if last.Strike != 350 {
			t.Fatalf("last strike = %.0f, want 350", last.Strike)
		}
	})

	t.Run("1st innings mid-match uses wicket time coeff 0.06", func(t *testing.T) {
		r := CalculateFirstInnings(PricingInput{
			Innings: 1, Score: 150, Wickets: 3, BallsLeft: 150,
		}, cfg)
		expectedT := (150.0 / 300.0) * (1.0 - 3*0.06)
		if abs(r.T-expectedT) > 0.0001 {
			t.Fatalf("T = %.4f, want %.4f", r.T, expectedT)
		}
		if r.S0 <= 150 {
			t.Fatalf("S0 = %.2f should exceed current score", r.S0)
		}
	})

	t.Run("1st innings all out is intrinsic only", func(t *testing.T) {
		r := CalculateFirstInnings(PricingInput{
			Innings: 1, Score: 236, Wickets: 10, BallsLeft: 24,
		}, cfg)
		for _, sp := range r.Chain {
			expected := 236.0 - sp.Strike
			if expected < 0 {
				expected = 0
			}
			if sp.Premium != expected {
				t.Fatalf("strike %.0f: got %.2f want %.2f", sp.Strike, sp.Premium, expected)
			}
		}
	})

	t.Run("2nd innings chase HTML projection", func(t *testing.T) {
		r := CalculateSecondInnings(PricingInput{
			Innings: 2, Score: 0, Wickets: 0, BallsBowled: 0, TargetScore: 280,
		}, cfg)
		if len(r.Chain) != 35 {
			t.Fatalf("expected 35 strikes, got %d", len(r.Chain))
		}
		if r.Sigma != 0.26 || r.R != 0.18 {
			t.Fatalf("sigma/r = %.2f/%.2f, want 0.26/0.18", r.Sigma, r.R)
		}
		if r.S0 <= 0 || r.S0 > 280 {
			t.Fatalf("S0 = %.2f should be in (0, 280]", r.S0)
		}
		// Strikes at/above target use max(score-K,0) → 0 at start.
		for _, sp := range r.Chain {
			if sp.Strike >= 280 && sp.Premium != 0 {
				t.Fatalf("strike %.0f >= target should be 0, got %.2f", sp.Strike, sp.Premium)
			}
		}
	})

	t.Run("2nd innings terminal finalize", func(t *testing.T) {
		r := CalculateSecondInnings(PricingInput{
			Innings: 2, Score: 236, Wickets: 10, BallsBowled: 276, TargetScore: 237,
		}, cfg)
		for _, sp := range r.Chain {
			expected := 236.0 - sp.Strike
			if expected < 0 {
				expected = 0
			}
			if sp.Premium != expected {
				t.Fatalf("terminal strike %.0f: got %.2f want %.2f", sp.Strike, sp.Premium, expected)
			}
		}
	})

	t.Run("CalculatePrice selects ODI via format", func(t *testing.T) {
		svc := NewService(NewMemoryRepository())
		got, err := svc.CalculatePrice(PriceCalculationInput{
			Format: "ODI", Innings: 1, CurrentScore: 0, WicketsLost: 0, BallsLeft: 150,
		})
		if err != nil {
			t.Fatalf("CalculatePrice: %v", err)
		}
		if got.MaxStrike != 350 {
			t.Fatalf("MaxStrike = %.0f, want 350 (ODI chain)", got.MaxStrike)
		}
		if len(got.OptionChain) != 35 {
			t.Fatalf("chain len = %d, want 35", len(got.OptionChain))
		}
	})

	t.Run("T20 unchanged when format empty and balls<=120", func(t *testing.T) {
		svc := NewService(NewMemoryRepository())
		got, err := svc.CalculatePrice(PriceCalculationInput{
			Innings: 1, CurrentScore: 0, WicketsLost: 0, BallsLeft: 119,
		})
		if err != nil {
			t.Fatalf("CalculatePrice: %v", err)
		}
		if got.MaxStrike != 250 {
			t.Fatalf("MaxStrike = %.0f, want 250 (T20)", got.MaxStrike)
		}
	})
}
