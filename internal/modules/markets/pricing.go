package markets

import (
	"math"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// PricingConfig holds tunable parameters for the option-chain engine.
// All values have sane T20 defaults but can be overridden per-call.
type PricingConfig struct {
	// 1st innings parameters
	Sigma1         float64 // volatility (default 0.30 T20 / 0.28 ODI)
	R1             float64 // risk-free rate (default 0.5 T20 / 0.18 ODI)
	MaxProjectedS0 float64 // ceiling on projected final score
	Alpha          float64 // capping factor
	DefaultRunRate float64 // fallback RR when no info
	MinRunRateBase float64 // floor for effective RR
	Beta           float64 // wicket-driven RR discount

	// 2nd innings parameters (T20 dynamic; ODI uses Sigma2 / R2 as fixed)
	SigmaBase  float64
	SigmaScale float64
	R2Base     float64
	R2TargetCo float64
	Sigma2     float64 // ODI 2nd-innings fixed vol (0.26); 0 → use T20 dynamic
	R2         float64 // ODI 2nd-innings fixed rate (0.18); 0 → use T20 dynamic

	// Shared
	WicketTimeCoeff float64 // T in (balls/total)*(1 - wickets*coeff); 0 → 0.1
	TrueZero        bool    // ODI HTML: intrinsic + timeValue*decay (no BS floor)
	ChaseExponent   float64 // ODI potential pow (0.85); 0 → 0.8 T20
	ChaseScale      float64 // ODI potential scale (0.6); 0 → 0.5 T20
	ChaseWicketDiv  float64 // ODI wicketFactor divisor (28); 0 → 25 T20

	// Chain configuration
	StrikeStep float64
	MaxStrike  float64
	BallsTotal int
}

// DefaultPricingConfig returns the standard T20 pricing configuration.
func DefaultPricingConfig() PricingConfig {
	return PricingConfig{
		Sigma1:         0.30,
		R1:             0.5,
		MaxProjectedS0: 260,
		Alpha:          1.0,
		DefaultRunRate: 1.2,
		MinRunRateBase: 1.0,
		Beta:           0.2,

		SigmaBase:  0.10,
		SigmaScale: 0.20,
		R2Base:     0.02,
		R2TargetCo: 0.0001,

		WicketTimeCoeff: 0.1,
		TrueZero:        false,
		ChaseExponent:   0.8,
		ChaseScale:      0.5,
		ChaseWicketDiv:  25,

		StrikeStep: 10,
		MaxStrike:  250,
		BallsTotal: matches.BallsT20,
	}
}

// DefaultODIPricingConfig matches the CricOptions ODI HTML calculator
// (true-zero premiums, 300 balls, strikes 10–350).
func DefaultODIPricingConfig() PricingConfig {
	return PricingConfig{
		Sigma1:         0.28,
		R1:             0.18,
		MaxProjectedS0: 440,
		Alpha:          0.8,
		DefaultRunRate: 1.22,
		MinRunRateBase: 1.10,
		Beta:           0.2,

		Sigma2: 0.26,
		R2:     0.18,

		WicketTimeCoeff: 0.06,
		TrueZero:        true,
		ChaseExponent:   0.85,
		ChaseScale:      0.6,
		ChaseWicketDiv:  28,

		StrikeStep: 10,
		MaxStrike:  350,
		BallsTotal: matches.BallsODI,
	}
}

// PricingConfigForFormat returns T20 or ODI engine config.
func PricingConfigForFormat(format string) PricingConfig {
	if matches.TotalBallsForFormat(format) == matches.BallsODI {
		return DefaultODIPricingConfig()
	}
	return DefaultPricingConfig()
}

// PricingConfigForODI keeps the previous helper name for call sites.
func PricingConfigForODI(_ PricingConfig) PricingConfig {
	return DefaultODIPricingConfig()
}

// IsODIFormat reports whether format (or implied ball totals) should use the ODI engine.
func IsODIFormat(format string, ballsLeft, ballsBowled int) bool {
	if matches.TotalBallsForFormat(format) == matches.BallsODI {
		return true
	}
	if strings.Contains(strings.ToUpper(strings.TrimSpace(format)), "ODI") {
		return true
	}
	return ballsLeft > matches.BallsT20 || ballsBowled > matches.BallsT20
}

// PricingInput is the unified input for both innings.
type PricingInput struct {
	Innings     int
	Score       int
	Wickets     int
	BallsLeft   int
	BallsBowled int
	TargetScore int
}

// FirstInningsResult holds the chain + metadata for a 1st innings calc.
type FirstInningsResult struct {
	S0               float64
	T                float64
	Chain            []StrikePremium
	EffectiveRunRate float64
}

// SecondInningsResult holds the chain + metadata for a 2nd innings calc.
type SecondInningsResult struct {
	S0    float64
	T     float64
	Sigma float64
	R     float64
	Chain []StrikePremium
}

func wicketTimeCoeff(cfg PricingConfig) float64 {
	if cfg.WicketTimeCoeff > 0 {
		return cfg.WicketTimeCoeff
	}
	return 0.1
}

func chaseExponent(cfg PricingConfig) float64 {
	if cfg.ChaseExponent > 0 {
		return cfg.ChaseExponent
	}
	return 0.8
}

func chaseScale(cfg PricingConfig) float64 {
	if cfg.ChaseScale > 0 {
		return cfg.ChaseScale
	}
	return 0.5
}

func chaseWicketDiv(cfg PricingConfig) float64 {
	if cfg.ChaseWicketDiv > 0 {
		return cfg.ChaseWicketDiv
	}
	return 25
}

// adjustedPremiumODI implements the HTML true-zero premium:
// intrinsic + timeValue * (1 - wickets/10), never floored to max(bs, intrinsic).
func adjustedPremiumODI(S0, K, T, sigma, r float64, wickets, score, ballsLeft int) float64 {
	if wickets >= 10 || ballsLeft <= 0 {
		return math.Max(float64(score)-K, 0)
	}
	if T <= 0 || S0 <= 0 || K <= 0 || sigma <= 0 {
		return math.Max(S0-K, 0)
	}
	sqrtT := math.Sqrt(T)
	d1 := (math.Log(S0/K) + (r+0.5*sigma*sigma)*T) / (sigma * sqrtT)
	d2 := d1 - sigma*sqrtT
	bs := S0*normCdf(d1) - K*math.Exp(-r*T)*normCdf(d2)
	intrinsic := math.Max(S0-K, 0)
	timeValue := math.Max(bs-intrinsic, 0)
	decay := 1.0 - float64(wickets)/10.0
	premium := intrinsic + timeValue*decay
	if premium < 0 {
		return 0
	}
	return premium
}

// CalculateFirstInnings projects the 1st innings final score S0 and prices the chain.
func CalculateFirstInnings(in PricingInput, cfg PricingConfig) FirstInningsResult {
	if cfg.TrueZero || cfg.BallsTotal >= matches.BallsODI {
		return calculateFirstInningsODI(in, cfg)
	}
	return calculateFirstInningsT20(in, cfg)
}

func calculateFirstInningsODI(in PricingInput, cfg PricingConfig) FirstInningsResult {
	res := FirstInningsResult{}
	if in.Wickets < 0 || in.Wickets > 10 || in.BallsLeft < 0 || in.BallsLeft > cfg.BallsTotal {
		return res
	}
	if in.Score < 0 {
		return res
	}
	ballsBowled := cfg.BallsTotal - in.BallsLeft

	var effectiveRR float64
	if in.Score == 0 || ballsBowled == 0 {
		effectiveRR = cfg.DefaultRunRate
	} else {
		currRR := float64(in.Score) / float64(ballsBowled)
		effectiveRR = math.Max(currRR, cfg.MinRunRateBase-cfg.Beta*(float64(in.Wickets)/10.0))
	}
	res.EffectiveRunRate = effectiveRR

	baseProj := float64(in.BallsLeft) * effectiveRR
	remain := cfg.MaxProjectedS0 - float64(in.Score)
	if remain < 0 {
		remain = 0
	}
	cappedProj := remain / (1.0 + cfg.Alpha*(float64(in.BallsLeft)/float64(cfg.BallsTotal)))
	S0 := float64(in.Score) + math.Min(baseProj, cappedProj)
	res.S0 = S0

	T := (float64(in.BallsLeft) / float64(cfg.BallsTotal)) * (1.0 - float64(in.Wickets)*wicketTimeCoeff(cfg))
	if T < 0 {
		T = 0
	}
	res.T = T

	res.Chain = make([]StrikePremium, 0, int(cfg.MaxStrike/cfg.StrikeStep))
	for K := cfg.StrikeStep; K <= cfg.MaxStrike; K += cfg.StrikeStep {
		premium := adjustedPremiumODI(S0, K, T, cfg.Sigma1, cfg.R1, in.Wickets, in.Score, in.BallsLeft)
		res.Chain = append(res.Chain, StrikePremium{Strike: K, Premium: round2(premium)})
	}
	return res
}

func calculateFirstInningsT20(in PricingInput, cfg PricingConfig) FirstInningsResult {
	res := FirstInningsResult{}

	if in.Wickets < 0 || in.Wickets > 10 || in.BallsLeft < 0 || in.BallsLeft > cfg.BallsTotal {
		return res
	}
	if in.Score < 0 {
		return res
	}
	ballsBowled := cfg.BallsTotal - in.BallsLeft

	var effectiveRR float64
	if in.Score == 0 || ballsBowled == 0 {
		effectiveRR = cfg.DefaultRunRate
	} else {
		currRR := float64(in.Score) / float64(ballsBowled)
		effectiveRR = math.Max(currRR, cfg.MinRunRateBase-cfg.Beta*(float64(in.Wickets)/10.0))
	}
	res.EffectiveRunRate = effectiveRR

	baseProj := float64(in.BallsLeft) * effectiveRR
	remain := cfg.MaxProjectedS0 - float64(in.Score)
	if remain < 0 {
		remain = 0
	}
	cappedProj := remain / (1.0 + cfg.Alpha*(float64(in.BallsLeft)/float64(cfg.BallsTotal)))
	S0 := float64(in.Score) + math.Min(baseProj, cappedProj)
	res.S0 = S0

	T := (float64(in.BallsLeft) / float64(cfg.BallsTotal)) * (1.0 - float64(in.Wickets)*wicketTimeCoeff(cfg))
	if T < 0 {
		T = 0
	}
	res.T = T

	res.Chain = make([]StrikePremium, 0, int(cfg.MaxStrike/cfg.StrikeStep))
	for K := cfg.StrikeStep; K <= cfg.MaxStrike; K += cfg.StrikeStep {
		premium := 0.0
		if in.Wickets >= 10 || in.BallsLeft <= 0 {
			premium = math.Max(float64(in.Score)-K, 0)
		} else {
			if T > 0 && S0 > 0 && K > 0 {
				sqrtT := math.Sqrt(T)
				sigma := cfg.Sigma1
				d1 := (math.Log(S0/K) + (cfg.R1+0.5*sigma*sigma)*T) / (sigma * sqrtT)
				d2 := d1 - sigma*sqrtT
				bs := S0*normCdf(d1) - K*math.Exp(-cfg.R1*T)*normCdf(d2)
				intrinsic := math.Max(S0-K, 0)
				premium = math.Max(bs, intrinsic)
			} else {
				premium = math.Max(S0-K, 0)
			}
		}
		res.Chain = append(res.Chain, StrikePremium{Strike: K, Premium: round2(premium)})
	}

	return res
}

// CalculateSecondInnings prices a chase option chain for the 2nd innings.
func CalculateSecondInnings(in PricingInput, cfg PricingConfig) SecondInningsResult {
	if cfg.TrueZero || cfg.BallsTotal >= matches.BallsODI {
		return calculateSecondInningsODI(in, cfg)
	}
	return calculateSecondInningsT20(in, cfg)
}

func calculateSecondInningsODI(in PricingInput, cfg PricingConfig) SecondInningsResult {
	res := SecondInningsResult{}

	if in.TargetScore <= 0 || in.Score < 0 {
		return res
	}
	if in.Wickets < 0 || in.Wickets > 10 {
		return res
	}
	if in.BallsBowled < 0 || in.BallsBowled > cfg.BallsTotal {
		return res
	}
	ballsRemaining := cfg.BallsTotal - in.BallsBowled

	sigma := cfg.Sigma2
	if sigma <= 0 {
		sigma = 0.26
	}
	r := cfg.R2
	if r <= 0 {
		r = cfg.R1
	}
	if r <= 0 {
		r = 0.18
	}
	res.Sigma = sigma
	res.R = r

	// Terminal: all out or overs done → intrinsic only vs current score.
	if in.Wickets >= 10 || ballsRemaining <= 0 {
		res.S0 = float64(in.Score)
		res.T = 0
		res.Chain = make([]StrikePremium, 0, int(cfg.MaxStrike/cfg.StrikeStep))
		for K := cfg.StrikeStep; K <= cfg.MaxStrike; K += cfg.StrikeStep {
			premium := math.Max(float64(in.Score)-K, 0)
			res.Chain = append(res.Chain, StrikePremium{Strike: K, Premium: round2(premium)})
		}
		return res
	}

	wFactor := 1.0 + ((10.0 - float64(in.Wickets)) / chaseWicketDiv(cfg))
	potential := float64(in.TargetScore) *
		math.Pow(float64(ballsRemaining)/float64(cfg.BallsTotal), chaseExponent(cfg)) *
		chaseScale(cfg) * wFactor
	gap := float64(in.TargetScore - in.Score)
	if gap < 0 {
		gap = 0
	}
	S0 := math.Min(float64(in.TargetScore), float64(in.Score)+math.Min(gap, potential))
	res.S0 = S0

	T := (float64(ballsRemaining) / float64(cfg.BallsTotal)) * (1.0 - float64(in.Wickets)*wicketTimeCoeff(cfg))
	if T < 0 {
		T = 0
	}
	res.T = T

	res.Chain = make([]StrikePremium, 0, int(cfg.MaxStrike/cfg.StrikeStep))
	for K := cfg.StrikeStep; K <= cfg.MaxStrike; K += cfg.StrikeStep {
		var premium float64
		if K >= float64(in.TargetScore) {
			premium = math.Max(float64(in.Score)-K, 0)
		} else {
			premium = adjustedPremiumODI(S0, K, T, sigma, r, in.Wickets, in.Score, ballsRemaining)
		}
		res.Chain = append(res.Chain, StrikePremium{Strike: K, Premium: round2(premium)})
	}
	return res
}

func calculateSecondInningsT20(in PricingInput, cfg PricingConfig) SecondInningsResult {
	res := SecondInningsResult{}

	if in.TargetScore <= 0 || in.Score < 0 {
		return res
	}
	if in.Wickets < 0 || in.Wickets > 10 {
		return res
	}
	if in.BallsBowled < 0 || in.BallsBowled > cfg.BallsTotal {
		return res
	}
	ballsRemaining := cfg.BallsTotal - in.BallsBowled

	var S0 float64
	if ballsRemaining == 0 || in.Wickets == 10 {
		S0 = float64(in.Score)
	} else {
		wicketFactor := 1.0 + (10.0-float64(in.Wickets))/chaseWicketDiv(cfg)
		potential := float64(in.TargetScore) *
			math.Pow(float64(ballsRemaining)/float64(cfg.BallsTotal), chaseExponent(cfg)) *
			chaseScale(cfg) * wicketFactor
		gap := float64(in.TargetScore - in.Score)
		if gap < 0 {
			gap = 0
		}
		S0 = math.Max(float64(in.Score), float64(in.Score)+math.Min(gap, potential))
	}
	res.S0 = S0

	r := cfg.R2Base + float64(in.TargetScore)*cfg.R2TargetCo
	sigma := cfg.SigmaBase + cfg.SigmaScale*(float64(ballsRemaining)/float64(cfg.BallsTotal))
	res.R = r
	res.Sigma = sigma

	T := (float64(ballsRemaining) / float64(cfg.BallsTotal)) * (1.0 - float64(in.Wickets)*wicketTimeCoeff(cfg))
	if T < 0 {
		T = 0
	}
	res.T = T

	res.Chain = make([]StrikePremium, 0, int(cfg.MaxStrike/cfg.StrikeStep))
	for K := cfg.StrikeStep; K <= cfg.MaxStrike; K += cfg.StrikeStep {
		C := 0.0
		switch {
		case float64(in.Score) > float64(in.TargetScore):
			C = math.Max(float64(in.Score)-K, 0)
		case K > float64(in.TargetScore):
			C = 0
		case ballsRemaining == 0 || in.Wickets == 10:
			C = math.Max(0, math.Min(float64(in.TargetScore)-K, float64(in.Score)-K))
		default:
			if T > 0 && S0 > 0 && K > 0 && sigma > 0 {
				sqrtT := math.Sqrt(T)
				lnS0K := math.Log(S0 / K)
				sigmaSq := sigma * sigma
				numerator := lnS0K + (r+sigmaSq/2.0)*T
				denom := sigma * sqrtT
				if denom == 0 {
					denom = 1e-10
				}
				d1 := numerator / denom
				d2 := d1 - sigma*sqrtT
				Nd1 := normCdf(d1)
				Nd2 := normCdf(d2)
				discount := math.Exp(-r * T)
				C = math.Max(0, S0*Nd1-K*discount*Nd2)
			} else {
				C = math.Max(0, S0-K)
			}
		}
		res.Chain = append(res.Chain, StrikePremium{Strike: K, Premium: round2(C)})
	}

	return res
}

// AggregateChainToOHLC collapses an option chain into a single "fair" price plus
// OHLC-style stats that match the existing PriceResponse shape.
func AggregateChainToOHLC(chain []StrikePremium) (ltp, open, high, low float64) {
	if len(chain) == 0 {
		return 0, 0, 0, 0
	}

	premiums := make([]float64, len(chain))
	hi, lo := chain[0].Premium, chain[0].Premium
	sum := 0.0
	for i, sp := range chain {
		premiums[i] = sp.Premium
		sum += sp.Premium
		if sp.Premium > hi {
			hi = sp.Premium
		}
		if sp.Premium < lo {
			lo = sp.Premium
		}
	}
	avg := sum / float64(len(chain))

	openPrice := lo
	for _, p := range premiums {
		if p > 0 && p < openPrice {
			openPrice = p
		}
	}
	if openPrice <= 0 {
		openPrice = avg
	}

	return round2(avg), round2(openPrice), round2(hi), round2(lo)
}

func normCdf(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}
