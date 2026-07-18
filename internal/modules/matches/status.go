package matches

import (
	"sort"
	"strings"
)

const (
	StatusLive         = "live"
	StatusUpcoming     = "upcoming"
	StatusCompleted    = "completed"
	StatusInningsBreak = "innings_break"
	StatusSuspended    = "suspended"
	StatusAbandoned    = "abandoned"
)

// NormalizeStatus maps legacy/frontend labels to canonical backend statuses.
func NormalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusLive, "lIVE":
		return StatusLive
	case StatusUpcoming, "active", "scheduled":
		return StatusUpcoming
	case StatusCompleted, "finished", "done":
		return StatusCompleted
	case StatusInningsBreak, "innings break", "break":
		return StatusInningsBreak
	case StatusSuspended:
		return StatusSuspended
	case StatusAbandoned:
		return StatusAbandoned
	default:
		if status == "" {
			return StatusUpcoming
		}
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func isTerminalStatus(status string) bool {
	switch NormalizeStatus(status) {
	case StatusCompleted, StatusAbandoned:
		return true
	default:
		return false
	}
}

func isLiveStatus(status string) bool {
	return NormalizeStatus(status) == StatusLive
}

// SortHomeMatches returns matches ordered for the schedule strip: live first,
// then innings break, then upcoming (soonest start first), then everything else.
func SortHomeMatches(in []Match) []Match {
	out := make([]Match, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := homePriority(out[i].Status), homePriority(out[j].Status)
		if pi != pj {
			return pi < pj
		}
		if NormalizeStatus(out[i].Status) == StatusUpcoming {
			return out[i].StartTime.Before(out[j].StartTime)
		}
		return out[i].StartTime.After(out[j].StartTime)
	})
	return out
}

func homePriority(status string) int {
	switch NormalizeStatus(status) {
	case StatusLive:
		return 0
	case StatusInningsBreak:
		return 1
	case StatusUpcoming:
		return 2
	case StatusSuspended:
		return 3
	default:
		return 4
	}
}
