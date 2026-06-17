package matches

import (
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	StatusLive         = "live"
	StatusUpcoming     = "upcoming"
	StatusCompleted    = "completed"
	StatusInningsBreak = "innings_break"
	StatusSuspended    = "suspended"
	StatusAbandoned    = "abandoned"
)

var primaryLiveMatchID = mustObjectID("0000000000000000000000aa")

func mustObjectID(hex string) primitive.ObjectID {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		panic(err)
	}
	return id
}

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
// then upcoming, then everything else by start time descending.
func SortHomeMatches(in []Match) []Match {
	out := make([]Match, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := homePriority(out[i].Status), homePriority(out[j].Status)
		if pi != pj {
			return pi < pj
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
