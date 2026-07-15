package matches

import "strings"

const (
	BallsT20 = 120
	BallsODI = 300
)

// TotalBallsForFormat returns legal balls per innings for a match format.
func TotalBallsForFormat(format string) int {
	upper := strings.ToUpper(strings.TrimSpace(format))
	if strings.Contains(upper, "ODI") || strings.Contains(upper, "ONE") {
		return BallsODI
	}
	return BallsT20
}
