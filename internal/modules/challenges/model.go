package challenges

const (
	StatusLocked     = "LOCKED"
	StatusInProgress = "IN_PROGRESS"
	StatusComplete   = "COMPLETE"
)

// Challenge is the user-facing view of a challenge. Progress and Status are
// always derived server-side from trading activity — a client can never assert
// that a challenge is done.
type Challenge struct {
	ID          string  `json:"id"`
	AcademyID   string  `json:"academyId"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Target      int     `json:"target"`
	Progress    int     `json:"progress"`
	Reward      float64 `json:"reward"`
	Status      string  `json:"status"`
	Claimed     bool    `json:"claimed"`
	// LockedReason is set when a challenge cannot be verified from the data the
	// platform records today. Such a challenge is shown but never claimable,
	// rather than being silently gated on an unenforced check.
	LockedReason string `json:"lockedReason,omitempty"`
}

// Claimable reports whether a reward may still be paid out for this challenge.
// This is the single gate the claim path enforces.
func (c Challenge) Claimable() bool {
	return c.Status == StatusComplete && !c.Claimed && c.LockedReason == ""
}
