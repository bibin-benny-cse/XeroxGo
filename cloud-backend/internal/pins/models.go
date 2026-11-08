package pins

// PublicOrderResponse represents a sanitized customer-facing order status.
// It explicitly omits internal hashes, HMACs, or database secrets.
type PublicOrderResponse struct {
	OrderID       string `json:"order_id"`
	JobID         string `json:"job_id"`
	DisplayCode   string `json:"display_code"`
	Status        string `json:"status"`
	TotalPages    int    `json:"total_pages"`
	TotalSheets   int    `json:"total_sheets"`
	Copies        int    `json:"copies"`
	Duplex        string `json:"duplex"`
	ColorMode     string `json:"color_mode"`
	PricePaise    int64  `json:"price_paise"`
	PagesPrinted  int    `json:"pages_printed,omitempty"`
	SheetsPrinted int    `json:"sheets_printed,omitempty"`
	ErrorReason   string `json:"error_reason,omitempty"`
}

// CustomerOrderCreationResponse is returned ONCE to the customer upon initial order creation.
// It contains the freshly generated 4-digit pickup PIN and the customer order secret (proof of ownership).
type CustomerOrderCreationResponse struct {
	OrderID             string `json:"order_id"`
	JobID               string `json:"job_id"`
	PickupPIN           string `json:"pickup_pin"`
	OrderSecret         string `json:"order_secret"`
	PINExpiresAt        int64  `json:"pin_expires_at"`
	PINExpiresInSeconds int64  `json:"pin_expires_in_seconds"`
	DisplayCode         string `json:"display_code"`
	ColorMode           string `json:"color_mode"`
	TotalPages          int    `json:"total_pages"`
	TotalSheets         int    `json:"total_sheets"`
	Copies              int    `json:"copies"`
	Duplex              string `json:"duplex"`
	PricePerSheetPaise  int64  `json:"price_per_sheet_paise"`
	PricePerSidePaise   int64  `json:"price_per_side_paise"`
	UnitPricePaise      int64  `json:"unit_price_paise"`
	TotalPricePaise     int64  `json:"total_price_paise"`
	Status              string `json:"status"`
}

// PINRefreshRequest represents a request to generate a fresh pickup PIN for an unpaid order.
type PINRefreshRequest struct {
	OrderSecret string `json:"order_secret"`
}

// PINRefreshResponse is returned to the customer upon authorized PIN refresh.
type PINRefreshResponse struct {
	OrderID             string `json:"order_id"`
	PickupPIN           string `json:"pickup_pin"`
	PINExpiresAt        int64  `json:"pin_expires_at"`
	PINExpiresInSeconds int64  `json:"pin_expires_in_seconds"`
	Status              string `json:"status"`
}

// PINRedemptionRequest is submitted by an authenticated kiosk to redeem a customer's PIN.
type PINRedemptionRequest struct {
	PIN       string `json:"pin"`
	SessionID string `json:"session_id,omitempty"`
}

// PINRedemptionResponse confirms successful PIN redemption and creation of an active payment session.
// PIN redemption NEVER authorizes printing directly.
type PINRedemptionResponse struct {
	Status           string `json:"status"` // "ready_for_payment"
	SessionID        string `json:"session_id"`
	OrderID          string `json:"order_id"`
	KioskID          string `json:"kiosk_id"`
	PricePaise       int64  `json:"price_paise"`
	TotalPages       int    `json:"total_pages"`
	TotalSheets      int    `json:"total_sheets"`
	Copies           int    `json:"copies"`
	Duplex           string `json:"duplex"`
	ColorMode        string `json:"color_mode"`
	SessionExpiresAt int64  `json:"session_expires_at"`
}
