package positions

// PositionFilter narrows which positions a list query returns. Empty fields
// mean "do not filter on this dimension". UserID is zero-value (unset) means
// "do not filter on user" — used for the admin list-all endpoint.
type PositionFilter struct {
	UserID   string // ObjectID hex; empty = all users (admin)
	MatchID  string
	MarketID string
	Status   string // "open", "closed", or "" for both
}
