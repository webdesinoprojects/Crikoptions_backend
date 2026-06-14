package wallet

type FundingRequest struct {
	Amount float64 `json:"amount"`
	Reason string  `json:"reason"`
}

type FundingResponse struct {
	Wallet      Account     `json:"wallet"`
	LedgerEntry LedgerEntry `json:"ledgerEntry"`
}
