package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"xeroxgo/cloud-backend/internal/store"
)

// handlePublicPricing handles GET /api/pricing for the public portal
func (app *appServer) handlePublicPricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	pricing, err := app.db.GetPricing(r.Context())
	if err != nil {
		pricing = store.PricingConfig{
			BWPricePerSheetPaise:    200,
			ColorPricePerSheetPaise: 1000,
			UpdatedAt:               time.Now().UTC(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pricing)
}

// handleAdminPricing handles GET and PUT /api/admin/pricing
func (app *appServer) handleAdminPricing(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch r.Method {
	case http.MethodGet:
		pricing, err := app.db.GetPricing(r.Context())
		if err != nil {
			pricing = store.PricingConfig{
				BWPricePerSheetPaise:    200,
				ColorPricePerSheetPaise: 1000,
				UpdatedAt:               time.Now().UTC(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pricing)

	case http.MethodPut:
		var req struct {
			BWSimplexPricePaise    int64 `json:"bw_simplex_price_paise"`
			BWDuplexPricePaise     int64 `json:"bw_duplex_price_paise"`
			ColorSimplexPricePaise int64 `json:"color_simplex_price_paise"`
			ColorDuplexPricePaise  int64 `json:"color_duplex_price_paise"`
			ColorEnabled           bool  `json:"color_enabled"`
			// Backwards compatibility for single-price payloads
			BWPricePerSheetPaise    int64 `json:"bw_price_per_sheet_paise"`
			ColorPricePerSheetPaise int64 `json:"color_price_per_sheet_paise"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.BWSimplexPricePaise <= 0 && req.BWPricePerSheetPaise > 0 {
			req.BWSimplexPricePaise = req.BWPricePerSheetPaise
		}
		if req.BWDuplexPricePaise <= 0 {
			req.BWDuplexPricePaise = (req.BWSimplexPricePaise * 3) / 4
			if req.BWDuplexPricePaise <= 0 {
				req.BWDuplexPricePaise = 150
			}
		}
		if req.ColorSimplexPricePaise <= 0 && req.ColorPricePerSheetPaise > 0 {
			req.ColorSimplexPricePaise = req.ColorPricePerSheetPaise
		}
		if req.ColorDuplexPricePaise <= 0 {
			req.ColorDuplexPricePaise = (req.ColorSimplexPricePaise * 3) / 4
			if req.ColorDuplexPricePaise <= 0 {
				req.ColorDuplexPricePaise = 750
			}
		}

		if req.BWSimplexPricePaise <= 0 || req.BWDuplexPricePaise <= 0 || req.ColorSimplexPricePaise <= 0 || req.ColorDuplexPricePaise <= 0 {
			writeJSONError(w, http.StatusBadRequest, "prices must be greater than zero")
			return
		}

		if err := app.db.SetDetailedPricing(r.Context(), req.BWSimplexPricePaise, req.BWDuplexPricePaise, req.ColorSimplexPricePaise, req.ColorDuplexPricePaise, req.ColorEnabled); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save pricing: %v", err))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":                      "ok",
			"bw_simplex_price_paise":      req.BWSimplexPricePaise,
			"bw_duplex_price_paise":       req.BWDuplexPricePaise,
			"color_simplex_price_paise":   req.ColorSimplexPricePaise,
			"color_duplex_price_paise":    req.ColorDuplexPricePaise,
			"color_enabled":               req.ColorEnabled,
			"bw_price_per_sheet_paise":    req.BWSimplexPricePaise,
			"color_price_per_sheet_paise": req.ColorSimplexPricePaise,
		})

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAdminAnalytics handles GET /api/admin/analytics
func (app *appServer) handleAdminAnalytics(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "7d"
	}

	analytics, err := app.db.GetAnalytics(r.Context(), timeRange)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch analytics: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(analytics)
}

// handleAdminOrders handles GET /api/admin/orders
func (app *appServer) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	status := r.URL.Query().Get("status")
	search := r.URL.Query().Get("search")
	kioskID := r.URL.Query().Get("kiosk_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	filter := store.OrderHistoryFilter{
		Status:  status,
		Search:  search,
		KioskID: kioskID,
		Limit:   limit,
		Offset:  offset,
	}

	history, err := app.db.GetOrderHistory(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch order history: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(history)
}

// handleAdminOrdersExport handles GET /api/admin/orders/export (CSV download)
func (app *appServer) handleAdminOrdersExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	history, err := app.db.GetOrderHistory(r.Context(), store.OrderHistoryFilter{Limit: 2000})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to export orders: %v", err))
		return
	}

	filename := fmt.Sprintf("xeroxgo-orders-%s.csv", time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	writer := csv.NewWriter(w)
	defer writer.Flush()

	// Write CSV Header
	_ = writer.Write([]string{
		"Order ID",
		"Job ID",
		"Display Code",
		"Kiosk ID",
		"Date (UTC)",
		"Status",
		"Color Mode",
		"Pages",
		"Sheets",
		"Copies",
		"Duplex",
		"Amount (INR)",
		"Error Reason",
	})

	for _, item := range history.Orders {
		amountINR := fmt.Sprintf("%.2f", float64(item.PricePaise)/100.0)
		_ = writer.Write([]string{
			sanitizeCSVField(item.OrderID),
			sanitizeCSVField(item.JobID),
			sanitizeCSVField(item.DisplayCode),
			sanitizeCSVField(item.KioskID),
			item.CreatedAt.Format("2006-01-02 15:04:05"),
			sanitizeCSVField(string(item.Status)),
			sanitizeCSVField(item.ColorMode),
			strconv.Itoa(item.TotalPages),
			strconv.Itoa(item.TotalSheets),
			strconv.Itoa(item.Copies),
			sanitizeCSVField(string(item.Duplex)),
			amountINR,
			sanitizeCSVField(item.ErrorReason),
		})
	}
}

// sanitizeCSVField prevents Formula Injection (CSV injection) in spreadsheet applications
// by prefixing formula trigger characters (=, +, -, @, tab, CR) with a single quote.
func sanitizeCSVField(s string) string {
	if s == "" {
		return ""
	}
	first := s[0]
	if first == '=' || first == '+' || first == '-' || first == '@' || first == '\t' || first == '\r' {
		return "'" + s
	}
	return s
}
