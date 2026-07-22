package portfolio

import (
	"os"
	"strings"
	"sync"
	"time"

	// Embed the IANA database so the trading-day zone resolves identically on
	// scratch/distroless containers, which ship no /usr/share/zoneinfo.
	_ "time/tzdata"
)

// defaultTradingDayTimezone is IST: the product prices in Rs and its users
// trade Indian match schedules, so "today" must roll over at midnight IST and
// not at the server's UTC midnight (which lands at 05:30 IST).
const defaultTradingDayTimezone = "Asia/Kolkata"

var (
	tradingDayOnce sync.Once
	tradingDayLoc  *time.Location
)

// tradingDayLocation resolves the zone whose midnight starts a trading day.
// TRADING_DAY_TIMEZONE overrides it; an unparseable value falls back to IST
// rather than to the server zone, so a typo cannot silently reintroduce the
// UTC-midnight boundary.
func tradingDayLocation() *time.Location {
	tradingDayOnce.Do(func() {
		name := strings.TrimSpace(os.Getenv("TRADING_DAY_TIMEZONE"))
		if name == "" {
			name = defaultTradingDayTimezone
		}
		if loc, err := time.LoadLocation(name); err == nil {
			tradingDayLoc = loc
			return
		}
		if loc, err := time.LoadLocation(defaultTradingDayTimezone); err == nil {
			tradingDayLoc = loc
			return
		}
		tradingDayLoc = time.FixedZone("IST", 5*3600+30*60)
	})
	return tradingDayLoc
}

// isSameTradingDay reports whether a falls on the same trading day as b.
// A zero timestamp is never "today" — an unparseable openedAt/closedAt must not
// be counted into today's P&L.
func isSameTradingDay(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	loc := tradingDayLocation()
	aa := a.In(loc)
	bb := b.In(loc)
	return aa.Year() == bb.Year() && aa.Month() == bb.Month() && aa.Day() == bb.Day()
}
