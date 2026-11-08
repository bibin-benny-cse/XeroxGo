package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// PricingConfig represents comprehensive pricing for monochrome and color prints (simplex and duplex)
// along with hardware capabilities.
type PricingConfig struct {
	BWSimplexPricePaise     int64     `json:"bw_simplex_price_paise"`
	BWDuplexPricePaise      int64     `json:"bw_duplex_price_paise"`
	ColorSimplexPricePaise  int64     `json:"color_simplex_price_paise"`
	ColorDuplexPricePaise   int64     `json:"color_duplex_price_paise"`
	ColorEnabled            bool      `json:"color_enabled"`
	BWPricePerSheetPaise    int64     `json:"bw_price_per_sheet_paise"`
	ColorPricePerSheetPaise int64     `json:"color_price_per_sheet_paise"`
	BWPricePerSidePaise     int64     `json:"bw_price_per_side_paise,omitempty"`
	ColorPricePerSidePaise  int64     `json:"color_price_per_side_paise,omitempty"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// GetPricing retrieves the current pricing configuration.
func (s *Store) GetPricing(ctx context.Context) (PricingConfig, error) {
	if s == nil || s.db == nil {
		return PricingConfig{}, fmt.Errorf("get pricing: store is closed or nil")
	}
	if ctx == nil {
		return PricingConfig{}, fmt.Errorf("get pricing: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return PricingConfig{}, fmt.Errorf("get pricing: %w", err)
	}

	var legacyPrice, bwPrice, colorPrice, bwSimplex, bwDuplex, colorSimplex, colorDuplex int64
	var colorEnabledInt int
	var updatedNS int64
	query := "SELECT price_per_sheet_paise, bw_price_per_sheet_paise, color_price_per_sheet_paise, bw_simplex_price_paise, bw_duplex_price_paise, color_simplex_price_paise, color_duplex_price_paise, color_enabled, updated_at_ns FROM pricing WHERE id = 1;"
	err := s.db.QueryRowContext(ctx, query).Scan(
		&legacyPrice, &bwPrice, &colorPrice,
		&bwSimplex, &bwDuplex,
		&colorSimplex, &colorDuplex,
		&colorEnabledInt, &updatedNS,
	)
	if err == sql.ErrNoRows {
		return PricingConfig{}, fmt.Errorf("get pricing: pricing has not been configured")
	}
	if err != nil {
		return PricingConfig{}, fmt.Errorf("get pricing: %w", err)
	}

	if bwSimplex <= 0 {
		if bwPrice > 0 {
			bwSimplex = bwPrice
		} else if legacyPrice > 0 {
			bwSimplex = legacyPrice
		} else {
			bwSimplex = 200
		}
	}
	if bwDuplex <= 0 {
		bwDuplex = (bwSimplex * 3) / 4
		if bwDuplex <= 0 {
			bwDuplex = 150
		}
	}
	if colorSimplex <= 0 {
		if colorPrice > 0 {
			colorSimplex = colorPrice
		} else {
			colorSimplex = 1000
		}
	}
	if colorDuplex <= 0 {
		colorDuplex = (colorSimplex * 3) / 4
		if colorDuplex <= 0 {
			colorDuplex = 750
		}
	}

	return PricingConfig{
		BWSimplexPricePaise:     bwSimplex,
		BWDuplexPricePaise:      bwDuplex,
		ColorSimplexPricePaise:  colorSimplex,
		ColorDuplexPricePaise:   colorDuplex,
		ColorEnabled:            colorEnabledInt == 1,
		BWPricePerSheetPaise:    bwSimplex,
		ColorPricePerSheetPaise: colorSimplex,
		BWPricePerSidePaise:     bwSimplex,
		ColorPricePerSidePaise:  colorSimplex,
		UpdatedAt:               time.Unix(0, updatedNS).UTC(),
	}, nil
}

// SetDetailedPricing updates or inserts the complete 4-tier pricing and color hardware configuration.
func (s *Store) SetDetailedPricing(ctx context.Context, bwSimplex, bwDuplex, colorSimplex, colorDuplex int64, colorEnabled bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set pricing: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("set pricing: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("set pricing: %w", err)
	}
	if bwSimplex <= 0 {
		return fmt.Errorf("set pricing: bwSimplex must be > 0, got %d", bwSimplex)
	}
	if bwDuplex <= 0 {
		return fmt.Errorf("set pricing: bwDuplex must be > 0, got %d", bwDuplex)
	}
	if colorSimplex <= 0 {
		return fmt.Errorf("set pricing: colorSimplex must be > 0, got %d", colorSimplex)
	}
	if colorDuplex <= 0 {
		return fmt.Errorf("set pricing: colorDuplex must be > 0, got %d", colorDuplex)
	}

	colorEnabledInt := 0
	if colorEnabled {
		colorEnabledInt = 1
	}

	updatedNS := time.Now().UTC().UnixNano()
	query := `
INSERT INTO pricing (id, price_per_sheet_paise, bw_price_per_sheet_paise, color_price_per_sheet_paise,
                     bw_simplex_price_paise, bw_duplex_price_paise,
                     color_simplex_price_paise, color_duplex_price_paise,
                     color_enabled, updated_at_ns)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    price_per_sheet_paise = excluded.price_per_sheet_paise,
    bw_price_per_sheet_paise = excluded.bw_price_per_sheet_paise,
    color_price_per_sheet_paise = excluded.color_price_per_sheet_paise,
    bw_simplex_price_paise = excluded.bw_simplex_price_paise,
    bw_duplex_price_paise = excluded.bw_duplex_price_paise,
    color_simplex_price_paise = excluded.color_simplex_price_paise,
    color_duplex_price_paise = excluded.color_duplex_price_paise,
    color_enabled = excluded.color_enabled,
    updated_at_ns = excluded.updated_at_ns;
`
	_, err := s.db.ExecContext(ctx, s.rebind(query),
		bwSimplex, bwSimplex, colorSimplex,
		bwSimplex, bwDuplex,
		colorSimplex, colorDuplex,
		colorEnabledInt, updatedNS)
	if err != nil {
		return fmt.Errorf("set pricing: %w", err)
	}
	return nil
}

// SetPricing updates or inserts the dual pricing configuration (backward compatible).
func (s *Store) SetPricing(ctx context.Context, bwPrice, colorPrice int64) error {
	bwDuplex := (bwPrice * 3) / 4
	if bwDuplex <= 0 {
		bwDuplex = 150
	}
	colorDuplex := (colorPrice * 3) / 4
	if colorDuplex <= 0 {
		colorDuplex = 750
	}
	return s.SetDetailedPricing(ctx, bwPrice, bwDuplex, colorPrice, colorDuplex, false)
}

// DailyTrendItem represents volume and revenue aggregated by calendar day.
type DailyTrendItem struct {
	Date         string `json:"date"` // "YYYY-MM-DD"
	RevenuePaise int64  `json:"revenue_paise"`
	SheetsCount  int64  `json:"sheets_count"`
	OrderCount   int64  `json:"order_count"`
}

// AnalyticsSummary contains high-level kiosk business KPIs and trend history.
type AnalyticsSummary struct {
	TotalRevenuePaise  int64            `json:"total_revenue_paise"`
	RevenueTodayPaise  int64            `json:"revenue_today_paise"`
	Revenue7dPaise     int64            `json:"revenue_7d_paise"`
	TotalSheetsPrinted int64            `json:"total_sheets_printed"`
	TotalPagesPrinted  int64            `json:"total_pages_printed"`
	TotalOrders        int64            `json:"total_orders"`
	CompletedOrders    int64            `json:"completed_orders"`
	FailedOrders       int64            `json:"failed_orders"`
	SuccessRatePercent float64          `json:"success_rate_percent"`
	AverageOrderPaise  int64            `json:"average_order_paise"`
	BWOrdersCount      int64            `json:"bw_orders_count"`
	ColorOrdersCount   int64            `json:"color_orders_count"`
	DailyTrends        []DailyTrendItem `json:"daily_trends"`
}

// GetAnalytics calculates comprehensive kiosk metrics and trend buckets.
func (s *Store) GetAnalytics(ctx context.Context, timeRange string) (*AnalyticsSummary, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get analytics: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get analytics: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get analytics: %w", err)
	}

	now := time.Now().UTC()
	todayStartNS := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixNano()
	sevenDaysAgoNS := now.AddDate(0, 0, -7).UnixNano()

	// 1. Overall Completed stats: Total Revenue, Total Sheets, Total Pages, Completed Orders Count
	var totalRevenue, totalSheets, totalPages, completedCount int64
	rowComp := s.db.QueryRowContext(ctx, `
SELECT
    COALESCE(SUM(o.price_paise), 0),
    COALESCE(SUM(p.total_sheets), 0),
    COALESCE(SUM(p.total_pages), 0),
    COUNT(*)
FROM orders o
LEFT JOIN print_configs p ON o.order_id = p.order_id
WHERE o.status = 'COMPLETED';
`)
	if err := rowComp.Scan(&totalRevenue, &totalSheets, &totalPages, &completedCount); err != nil {
		return nil, fmt.Errorf("get analytics: completed stats: %w", err)
	}

	// 2. Today's Revenue
	var revToday int64
	_ = s.db.QueryRowContext(ctx, s.rebind(`
SELECT COALESCE(SUM(price_paise), 0)
FROM orders
WHERE status = 'COMPLETED' AND created_at_ns >= ?;
`), todayStartNS).Scan(&revToday)

	// 3. Last 7 Days Revenue
	var rev7d int64
	_ = s.db.QueryRowContext(ctx, s.rebind(`
SELECT COALESCE(SUM(price_paise), 0)
FROM orders
WHERE status = 'COMPLETED' AND created_at_ns >= ?;
`), sevenDaysAgoNS).Scan(&rev7d)

	// 4. Total orders count
	var totalOrders int64
	_ = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders;").Scan(&totalOrders)

	// 5. Failed orders count
	var failedOrders int64
	_ = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders WHERE status IN ('PAUSED_ERROR', 'FAILED_REFUNDABLE');").Scan(&failedOrders)

	// 6. B&W vs Color counts
	var bwCount, colorCount int64
	rowsColor, err := s.db.QueryContext(ctx, `
SELECT COALESCE(p.color_mode, 'BW'), COUNT(*)
FROM orders o
LEFT JOIN print_configs p ON o.order_id = p.order_id
GROUP BY p.color_mode;
`)
	if err == nil {
		defer rowsColor.Close()
		for rowsColor.Next() {
			var mode string
			var count int64
			if err := rowsColor.Scan(&mode, &count); err == nil {
				if strings.ToUpper(mode) == "COLOR" {
					colorCount += count
				} else {
					bwCount += count
				}
			}
		}
	}

	// Calculate Success Rate %
	var successRate float64
	if totalOrders > 0 {
		successRate = (float64(completedCount) / float64(totalOrders)) * 100.0
	}

	// Calculate Average Order Value
	var aov int64
	if completedCount > 0 {
		aov = totalRevenue / completedCount
	}

	// 7. Daily Trends for chart (last 7 or 14 days)
	daysCount := 7
	if timeRange == "30d" {
		daysCount = 30
	} else if timeRange == "14d" {
		daysCount = 14
	}

	trendMap := make(map[string]*DailyTrendItem)
	var trendDays []string
	for i := daysCount - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		item := &DailyTrendItem{Date: d}
		trendMap[d] = item
		trendDays = append(trendDays, d)
	}

	trendStartNS := now.AddDate(0, 0, -daysCount).UnixNano()
	rowsTrend, err := s.db.QueryContext(ctx, s.rebind(`
SELECT o.created_at_ns, o.price_paise, COALESCE(p.total_sheets, 1)
FROM orders o
LEFT JOIN print_configs p ON o.order_id = p.order_id
WHERE o.status = 'COMPLETED' AND o.created_at_ns >= ?;
`), trendStartNS)
	if err == nil {
		defer rowsTrend.Close()
		for rowsTrend.Next() {
			var cNS, pPaise, tSheets int64
			if err := rowsTrend.Scan(&cNS, &pPaise, &tSheets); err == nil {
				dateStr := time.Unix(0, cNS).UTC().Format("2006-01-02")
				if item, ok := trendMap[dateStr]; ok {
					item.RevenuePaise += pPaise
					item.SheetsCount += tSheets
					item.OrderCount++
				}
			}
		}
	}

	var dailyTrends []DailyTrendItem
	for _, d := range trendDays {
		dailyTrends = append(dailyTrends, *trendMap[d])
	}

	return &AnalyticsSummary{
		TotalRevenuePaise:  totalRevenue,
		RevenueTodayPaise:  revToday,
		Revenue7dPaise:     rev7d,
		TotalSheetsPrinted: totalSheets,
		TotalPagesPrinted:  totalPages,
		TotalOrders:        totalOrders,
		CompletedOrders:    completedCount,
		FailedOrders:       failedOrders,
		SuccessRatePercent: successRate,
		AverageOrderPaise:  aov,
		BWOrdersCount:      bwCount,
		ColorOrdersCount:   colorCount,
		DailyTrends:        dailyTrends,
	}, nil
}

// OrderHistoryFilter encapsulates filtering and pagination parameters for orders.
type OrderHistoryFilter struct {
	Status  string // empty = all
	Search  string // search order_id, job_id, or display_code
	KioskID string
	Limit   int
	Offset  int
}

// OrderHistoryItem represents an order record enriched with print configuration and progress.
type OrderHistoryItem struct {
	OrderID       string               `json:"order_id"`
	JobID         string               `json:"job_id"`
	KioskID       string               `json:"kiosk_id"`
	DisplayCode   string               `json:"display_code"`
	Status        contracts.JobStatus  `json:"status"`
	PricePaise    int64                `json:"price_paise"`
	ColorMode     string               `json:"color_mode"`
	TotalPages    int                  `json:"total_pages"`
	TotalSheets   int                  `json:"total_sheets"`
	Copies        int                  `json:"copies"`
	Duplex        contracts.DuplexMode `json:"duplex"`
	StartFromPage int                  `json:"start_from_page"`
	PagesPrinted  int                  `json:"pages_printed"`
	SheetsPrinted int                  `json:"sheets_printed"`
	ErrorReason   string               `json:"error_reason,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

// OrderHistoryResult returns the slice of orders and total matching count.
type OrderHistoryResult struct {
	Orders []OrderHistoryItem `json:"orders"`
	Total  int                `json:"total"`
}

// GetOrderHistory queries orders matching the specified filter criteria with pagination.
func (s *Store) GetOrderHistory(ctx context.Context, filter OrderHistoryFilter) (*OrderHistoryResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order history: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order history: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get order history: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	whereClauses := []string{"1=1"}
	var args []interface{}

	if strings.TrimSpace(filter.Status) != "" {
		whereClauses = append(whereClauses, "o.status = ?")
		args = append(args, strings.TrimSpace(filter.Status))
	}
	if strings.TrimSpace(filter.KioskID) != "" {
		whereClauses = append(whereClauses, "o.kiosk_id = ?")
		args = append(args, strings.TrimSpace(filter.KioskID))
	}
	if strings.TrimSpace(filter.Search) != "" {
		searchPattern := "%" + strings.TrimSpace(filter.Search) + "%"
		whereClauses = append(whereClauses, "(o.order_id LIKE ? OR o.job_id LIKE ? OR o.display_code LIKE ?)")
		args = append(args, searchPattern, searchPattern, searchPattern)
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	// 1. Total count
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM orders o WHERE %s;", whereSQL)
	var total int
	err := s.db.QueryRowContext(ctx, s.rebind(countQuery), args...).Scan(&total)
	if err != nil {
		return nil, fmt.Errorf("get order history: count: %w", err)
	}

	// 2. Query items
	dataQuery := fmt.Sprintf(`
SELECT
    o.order_id, o.job_id, o.kiosk_id, o.display_code, o.status, o.price_paise, o.created_at_ns, o.updated_at_ns,
    COALESCE(p.total_pages, 0), COALESCE(p.total_sheets, 0), COALESCE(p.copies, 1),
    COALESCE(p.duplex, 'SIMPLEX'), COALESCE(p.start_from_page, 1), COALESCE(p.color_mode, 'BW'),
    COALESCE(j.pages_printed, 0), COALESCE(j.sheets_printed, 0), COALESCE(j.error_reason, '')
FROM orders o
LEFT JOIN print_configs p ON o.order_id = p.order_id
LEFT JOIN job_progress j ON o.job_id = j.job_id
WHERE %s
ORDER BY o.created_at_ns DESC
LIMIT ? OFFSET ?;
`, whereSQL)

	queryArgs := append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, s.rebind(dataQuery), queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("get order history: query: %w", err)
	}
	defer rows.Close()

	var items []OrderHistoryItem
	for rows.Next() {
		var (
			orderID, jobID, kioskID, displayCode, statusStr string
			pricePaise, createdNS, updatedNS                int64
			totalPages, totalSheets, copies, startFromPage  int
			duplexStr, colorMode                            string
			pagesPrinted, sheetsPrinted                     int
			errorReason                                     string
		)
		err := rows.Scan(
			&orderID, &jobID, &kioskID, &displayCode, &statusStr, &pricePaise, &createdNS, &updatedNS,
			&totalPages, &totalSheets, &copies, &duplexStr, &startFromPage, &colorMode,
			&pagesPrinted, &sheetsPrinted, &errorReason,
		)
		if err != nil {
			return nil, fmt.Errorf("get order history: scan: %w", err)
		}

		items = append(items, OrderHistoryItem{
			OrderID:       orderID,
			JobID:         jobID,
			KioskID:       kioskID,
			DisplayCode:   displayCode,
			Status:        contracts.JobStatus(statusStr),
			PricePaise:    pricePaise,
			ColorMode:     colorMode,
			TotalPages:    totalPages,
			TotalSheets:   totalSheets,
			Copies:        copies,
			Duplex:        contracts.DuplexMode(duplexStr),
			StartFromPage: startFromPage,
			PagesPrinted:  pagesPrinted,
			SheetsPrinted: sheetsPrinted,
			ErrorReason:   errorReason,
			CreatedAt:     time.Unix(0, createdNS).UTC(),
			UpdatedAt:     time.Unix(0, updatedNS).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get order history: rows: %w", err)
	}

	if items == nil {
		items = []OrderHistoryItem{}
	}

	return &OrderHistoryResult{
		Orders: items,
		Total:  total,
	}, nil
}
