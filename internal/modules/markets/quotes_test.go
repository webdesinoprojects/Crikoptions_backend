package markets

import "testing"

func TestQuoteFromPremiumMatchesTerminalSpreadBands(t *testing.T) {
	tests := []struct {
		name    string
		premium float64
		wantBid float64
		wantAsk float64
	}{
		{name: "cheap strike", premium: 4.80, wantBid: 4.75, wantAsk: 4.85},
		{name: "mid strike", premium: 12.00, wantBid: 11.75, wantAsk: 12.25},
		{name: "wide strike", premium: 24.00, wantBid: 23.50, wantAsk: 24.50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBid, gotAsk := quoteFromPremium(tt.premium)
			if gotBid != tt.wantBid || gotAsk != tt.wantAsk {
				t.Fatalf("quoteFromPremium(%.2f) = %.2f/%.2f, want %.2f/%.2f", tt.premium, gotBid, gotAsk, tt.wantBid, tt.wantAsk)
			}
		})
	}
}
