package challenges

type Challenge struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Target      int    `json:"target"`
	Progress    int    `json:"progress"`
	XP          int    `json:"xp"`
	Status      string `json:"status"` // "LOCKED", "IN_PROGRESS", "COMPLETE"
}
