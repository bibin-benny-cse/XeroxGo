package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
	"xeroxgo/pkg/contracts"
)

type Store struct {
	db      *sql.DB
	dialect string
}

func (s *Store) rebind(query string) string {
	if s == nil || s.dialect != "postgres" {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	paramIdx := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			b.WriteString(fmt.Sprintf("$%d", paramIdx))
			paramIdx++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

type Order struct {
	OrderID    string
	Command    contracts.PrintJobCommand
	Status     contracts.JobStatus
	PricePaise int64
	ColorMode  string // "BW" or "COLOR"
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Open opens a database at dbPath. If dbPath starts with "postgres://" or "postgresql://",
// it connects to a PostgreSQL instance (e.g. Supabase). Otherwise it connects to SQLite,
// verifies durability pragmas, initializes schema tables, and returns a Store.
func Open(dbPath string) (*Store, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("open store: dbPath cannot be empty")
	}

	isPostgres := strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://")
	if isPostgres {
		config, err := pgx.ParseConfig(dbPath)
		if err != nil {
			return nil, fmt.Errorf("open store: postgres parse config: %w", err)
		}
		// For connection poolers like Supabase transaction mode (PgBouncer/Supavisor),
		// prepared statements must not be cached to avoid SQLSTATE 42P05.
		config.DefaultQueryExecMode = pgx.QueryExecModeExec
		db := stdlib.OpenDB(*config)
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: postgres ping failed: %w", err)
		}

		schema := `
CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL UNIQUE,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS print_configs (
    order_id TEXT PRIMARY KEY
        REFERENCES orders(order_id) ON DELETE CASCADE,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    color_mode TEXT NOT NULL DEFAULT 'BW'
);

CREATE TABLE IF NOT EXISTS pricing (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    price_per_sheet_paise BIGINT NOT NULL
        CHECK (price_per_sheet_paise > 0),
    bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_price_per_sheet_paise > 0),
    color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_price_per_sheet_paise > 0),
    bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_simplex_price_paise > 0),
    bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150
        CHECK (bw_duplex_price_paise > 0),
    color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_simplex_price_paise > 0),
    color_duplex_price_paise BIGINT NOT NULL DEFAULT 750
        CHECK (color_duplex_price_paise > 0),
    color_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_webhooks (
    webhook_id TEXT PRIMARY KEY,
    received_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_progress (
    job_id TEXT PRIMARY KEY
        REFERENCES orders(job_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS ads (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    ad_type TEXT NOT NULL DEFAULT 'gradient',
    image_url TEXT NOT NULL DEFAULT '',
    gradient_c0 TEXT NOT NULL DEFAULT '#1E3A8A',
    gradient_c1 TEXT NOT NULL DEFAULT '#2563EB',
    gradient_c2 TEXT NOT NULL DEFAULT '#4F46E5',
    link_url TEXT NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    username TEXT NOT NULL REFERENCES admin_users(username) ON DELETE CASCADE,
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS conversions (
    id TEXT PRIMARY KEY,
    token TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);
CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);
`
		if _, err := db.Exec(schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: postgres schema init: %w", err)
		}

		pgMigrations := []string{
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_duplex_price_paise BIGINT NOT NULL DEFAULT 750;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_enabled INTEGER NOT NULL DEFAULT 0;",
			"ALTER TABLE print_configs ADD COLUMN IF NOT EXISTS color_mode TEXT NOT NULL DEFAULT 'BW';",
			"CREATE TABLE IF NOT EXISTS conversions (id TEXT PRIMARY KEY, token TEXT NOT NULL, original_filename TEXT NOT NULL, file_size BIGINT NOT NULL, status TEXT NOT NULL, error_message TEXT NOT NULL DEFAULT '', input_path TEXT NOT NULL, output_path TEXT NOT NULL DEFAULT '', created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, expires_at_ns BIGINT NOT NULL);",
			"CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);",
			"CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);",
		}
		for _, mig := range pgMigrations {
			if _, err := db.Exec(mig); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("open store: postgres migration: %w", err)
			}
		}

		return &Store{db: db, dialect: "postgres"}, nil
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	db.SetMaxOpenConns(1)

	// Configure SQLite durability settings
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=FULL;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: configure %s: %w", pragma, err)
		}
	}

	// Verify durability settings
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: read journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		_ = db.Close()
		return nil, fmt.Errorf("open store: expected journal_mode=wal, got %q", journalMode)
	}

	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&synchronous); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: read synchronous: %w", err)
	}
	if synchronous != 2 {
		_ = db.Close()
		return nil, fmt.Errorf("open store: expected synchronous=2, got %d", synchronous)
	}

	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: read foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		_ = db.Close()
		return nil, fmt.Errorf("open store: expected foreign_keys=1, got %d", foreignKeys)
	}

	// Create required schema tables
	schema := `
CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL UNIQUE,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS print_configs (
    order_id TEXT PRIMARY KEY
        REFERENCES orders(order_id) ON DELETE CASCADE,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    color_mode TEXT NOT NULL DEFAULT 'BW'
);

CREATE TABLE IF NOT EXISTS pricing (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    price_per_sheet_paise BIGINT NOT NULL
        CHECK (price_per_sheet_paise > 0),
    bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_price_per_sheet_paise > 0),
    color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_price_per_sheet_paise > 0),
    bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_simplex_price_paise > 0),
    bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150
        CHECK (bw_duplex_price_paise > 0),
    color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_simplex_price_paise > 0),
    color_duplex_price_paise BIGINT NOT NULL DEFAULT 750
        CHECK (color_duplex_price_paise > 0),
    color_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_webhooks (
    webhook_id TEXT PRIMARY KEY,
    received_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_progress (
    job_id TEXT PRIMARY KEY
        REFERENCES orders(job_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS ads (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    ad_type TEXT NOT NULL DEFAULT 'gradient',
    image_url TEXT NOT NULL DEFAULT '',
    gradient_c0 TEXT NOT NULL DEFAULT '#1E3A8A',
    gradient_c1 TEXT NOT NULL DEFAULT '#2563EB',
    gradient_c2 TEXT NOT NULL DEFAULT '#4F46E5',
    link_url TEXT NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    username TEXT NOT NULL REFERENCES admin_users(username) ON DELETE CASCADE,
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS conversions (
    id TEXT PRIMARY KEY,
    token TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);
CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: create schema: %w", err)
	}

	if err := migrateSQLiteSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: sqlite migration: %w", err)
	}

	return &Store{db: db, dialect: "sqlite"}, nil
}

func migrateSQLiteSchema(db *sql.DB) error {
	colsPricing := make(map[string]bool)
	rows, err := db.Query("PRAGMA table_info(pricing);")
	if err == nil {
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt *string
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				colsPricing[name] = true
			}
		}
		_ = rows.Close()
	}
	if !colsPricing["bw_price_per_sheet_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN bw_price_per_sheet_paise INTEGER NOT NULL DEFAULT 200;")
	}
	if !colsPricing["color_price_per_sheet_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_price_per_sheet_paise INTEGER NOT NULL DEFAULT 1000;")
	}
	if !colsPricing["bw_simplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN bw_simplex_price_paise INTEGER NOT NULL DEFAULT 200;")
	}
	if !colsPricing["bw_duplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN bw_duplex_price_paise INTEGER NOT NULL DEFAULT 150;")
	}
	if !colsPricing["color_simplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_simplex_price_paise INTEGER NOT NULL DEFAULT 1000;")
	}
	if !colsPricing["color_duplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_duplex_price_paise INTEGER NOT NULL DEFAULT 750;")
	}
	if !colsPricing["color_enabled"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_enabled INTEGER NOT NULL DEFAULT 0;")
	}

	colsConfigs := make(map[string]bool)
	rowsCfg, err := db.Query("PRAGMA table_info(print_configs);")
	if err == nil {
		for rowsCfg.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt *string
			if err := rowsCfg.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				colsConfigs[name] = true
			}
		}
		_ = rowsCfg.Close()
	}
	if !colsConfigs["color_mode"] {
		_, _ = db.Exec("ALTER TABLE print_configs ADD COLUMN color_mode TEXT NOT NULL DEFAULT 'BW';")
	}

	_, _ = db.Exec(`
CREATE TABLE IF NOT EXISTS conversions (
    id TEXT PRIMARY KEY,
    token TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);
CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);
`)
	return nil
}

// Close closes the underlying SQLite database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// CreateOrder persists an order and its associated print configuration atomically.
func (s *Store) CreateOrder(ctx context.Context, order Order) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create order: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create order: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("create order: %w", err)
	}

	// Validation
	if strings.TrimSpace(order.OrderID) == "" {
		return fmt.Errorf("create order: order_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.JobID) == "" {
		return fmt.Errorf("create order: command.job_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.KioskID) == "" {
		return fmt.Errorf("create order: command.kiosk_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.DisplayCode) == "" {
		return fmt.Errorf("create order: command.display_code cannot be empty")
	}
	if order.Command.TotalPages < 1 {
		return fmt.Errorf("create order: total_pages must be >= 1, got %d", order.Command.TotalPages)
	}
	if order.Command.TotalSheets < 1 {
		return fmt.Errorf("create order: total_sheets must be >= 1, got %d", order.Command.TotalSheets)
	}
	if order.Command.Copies < 1 {
		return fmt.Errorf("create order: copies must be >= 1, got %d", order.Command.Copies)
	}
	if order.Command.StartFromPage < 1 {
		return fmt.Errorf("create order: start_from_page must be >= 1, got %d", order.Command.StartFromPage)
	}
	if order.Command.StartFromPage > order.Command.TotalPages {
		return fmt.Errorf("create order: start_from_page (%d) exceeds total_pages (%d)", order.Command.StartFromPage, order.Command.TotalPages)
	}
	if !isValidDuplex(order.Command.Duplex) {
		return fmt.Errorf("create order: invalid duplex mode %q", order.Command.Duplex)
	}
	if !isValidJobStatus(order.Status) {
		return fmt.Errorf("create order: invalid job status %q", order.Status)
	}
	if order.PricePaise < 0 {
		return fmt.Errorf("create order: price_paise must be >= 0, got %d", order.PricePaise)
	}

	// Timestamp handling
	createdAt := order.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	} else {
		createdAt = createdAt.UTC()
	}

	updatedAt := order.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	} else {
		updatedAt = updatedAt.UTC()
	}

	createdNS := createdAt.UnixNano()
	updatedNS := updatedAt.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create order: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO orders (order_id, job_id, kiosk_id, display_code, status, price_paise, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
`),
		order.OrderID,
		order.Command.JobID,
		order.Command.KioskID,
		order.Command.DisplayCode,
		string(order.Status),
		order.PricePaise,
		createdNS,
		updatedNS,
	)
	if err != nil {
		return fmt.Errorf("create order: insert into orders: %w", err)
	}

	colorMode := strings.ToUpper(strings.TrimSpace(order.ColorMode))
	if colorMode == "" {
		colorMode = "BW"
	}

	_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO print_configs (order_id, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, color_mode)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
`),
		order.OrderID,
		order.Command.DownloadURL,
		order.Command.SHA256Checksum,
		order.Command.TotalPages,
		order.Command.TotalSheets,
		order.Command.Copies,
		string(order.Command.Duplex),
		order.Command.StartFromPage,
		colorMode,
	)
	if err != nil {
		return fmt.Errorf("create order: insert into print_configs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create order: commit tx: %w", err)
	}

	return nil
}

// GetOrder retrieves an order and its associated print configuration.
func (s *Store) GetOrder(ctx context.Context, orderID string) (*Order, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("get order: orderID cannot be empty")
	}

	query := `
SELECT
    o.order_id, o.job_id, o.kiosk_id, o.display_code, o.status, o.price_paise, o.created_at_ns, o.updated_at_ns,
    p.download_url, p.sha256_checksum, p.total_pages, p.total_sheets, p.copies, p.duplex, p.start_from_page,
    COALESCE(p.color_mode, 'BW')
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.order_id = ?;
`

	row := s.db.QueryRowContext(ctx, s.rebind(query), orderID)

	var (
		resOrderID    string
		jobID         string
		kioskID       string
		displayCode   string
		statusStr     string
		pricePaise    int64
		createdNS     int64
		updatedNS     int64
		downloadURL   string
		sha256Check   string
		totalPages    int
		totalSheets   int
		copies        int
		duplexStr     string
		startFromPage int
		colorMode     string
	)

	err := row.Scan(
		&resOrderID,
		&jobID,
		&kioskID,
		&displayCode,
		&statusStr,
		&pricePaise,
		&createdNS,
		&updatedNS,
		&downloadURL,
		&sha256Check,
		&totalPages,
		&totalSheets,
		&copies,
		&duplexStr,
		&startFromPage,
		&colorMode,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order: scan: %w", err)
	}

	result := &Order{
		OrderID: resOrderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    displayCode,
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Check,
			TotalPages:     totalPages,
			TotalSheets:    totalSheets,
			Copies:         copies,
			Duplex:         contracts.DuplexMode(duplexStr),
			StartFromPage:  startFromPage,
		},
		Status:     contracts.JobStatus(statusStr),
		PricePaise: pricePaise,
		ColorMode:  colorMode,
		CreatedAt:  time.Unix(0, createdNS).UTC(),
		UpdatedAt:  time.Unix(0, updatedNS).UTC(),
	}

	return result, nil
}

// UpdateOrderStatus updates an existing order's status and updated_at_ns.
func (s *Store) UpdateOrderStatus(ctx context.Context, orderID string, status contracts.JobStatus) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update order status: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update order status: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	if strings.TrimSpace(orderID) == "" {
		return fmt.Errorf("update order status: orderID cannot be empty")
	}
	if !isValidJobStatus(status) {
		return fmt.Errorf("update order status: invalid job status %q", status)
	}

	updatedNS := time.Now().UTC().UnixNano()

	res, err := s.db.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ?;
`), string(status), updatedNS, orderID)
	if err != nil {
		return fmt.Errorf("update order status: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update order status: check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update order status: order %q not found", orderID)
	}

	return nil
}

// SetPricePerSheetPaise stores or replaces the singleton per-sheet pricing.
func (s *Store) SetPricePerSheetPaise(ctx context.Context, pricePaise int64) error {
	cfg, _ := s.GetPricing(ctx)
	colorPrice := cfg.ColorPricePerSheetPaise
	if colorPrice <= 0 {
		colorPrice = 1000
	}
	return s.SetPricing(ctx, pricePaise, colorPrice)
}

// GetPricePerSheetPaise retrieves the configured per-sheet pricing.
func (s *Store) GetPricePerSheetPaise(ctx context.Context) (int64, error) {
	cfg, err := s.GetPricing(ctx)
	if err != nil {
		return 0, err
	}
	return cfg.BWPricePerSheetPaise, nil
}

// RecordWebhook atomically attempts to record a webhook ID.
// Returns (true, nil) if inserted for the first time, (false, nil) if already existed.
func (s *Store) RecordWebhook(ctx context.Context, webhookID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("record webhook: store is closed or nil")
	}
	if ctx == nil {
		return false, fmt.Errorf("record webhook: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("record webhook: %w", err)
	}
	if strings.TrimSpace(webhookID) == "" {
		return false, fmt.Errorf("record webhook: webhookID cannot be empty or whitespace-only")
	}

	receivedNS := time.Now().UTC().UnixNano()

	query := `
INSERT INTO payment_webhooks (webhook_id, received_at_ns)
VALUES (?, ?)
ON CONFLICT(webhook_id) DO NOTHING;
`
	res, err := s.db.ExecContext(ctx, s.rebind(query), webhookID, receivedNS)
	if err != nil {
		return false, fmt.Errorf("record webhook: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record webhook: check rows affected: %w", err)
	}

	return rows == 1, nil
}

// UpdateJobProgress validates and atomically persists the latest progress event for a job.
func (s *Store) UpdateJobProgress(ctx context.Context, event contracts.JobProgressEvent) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update job progress: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update job progress: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update job progress: %w", err)
	}

	// Validation
	if strings.TrimSpace(event.JobID) == "" {
		return fmt.Errorf("update job progress: event.job_id cannot be empty")
	}
	if strings.TrimSpace(event.KioskID) == "" {
		return fmt.Errorf("update job progress: event.kiosk_id cannot be empty")
	}
	if !isValidJobStatus(event.Status) {
		return fmt.Errorf("update job progress: invalid event status %q", event.Status)
	}
	if event.PagesPrinted < 0 {
		return fmt.Errorf("update job progress: pages_printed must be >= 0, got %d", event.PagesPrinted)
	}
	if event.SheetsPrinted < 0 {
		return fmt.Errorf("update job progress: sheets_printed must be >= 0, got %d", event.SheetsPrinted)
	}
	if event.Timestamp.IsZero() {
		return fmt.Errorf("update job progress: timestamp cannot be zero")
	}

	eventNS := event.Timestamp.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update job progress: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Check order exists and retrieve kiosk_id, total_pages, total_sheets
	var (
		orderKioskID string
		totalPages   int
		totalSheets  int
	)
	queryOrder := `
SELECT o.kiosk_id, p.total_pages, p.total_sheets
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.job_id = ?;
`
	err = tx.QueryRowContext(ctx, s.rebind(queryOrder), event.JobID).Scan(&orderKioskID, &totalPages, &totalSheets)
	if err == sql.ErrNoRows {
		return fmt.Errorf("update job progress: job %q not found in orders", event.JobID)
	}
	if err != nil {
		return fmt.Errorf("update job progress: query order: %w", err)
	}

	// 2. Verify kiosk ID matches
	if orderKioskID != event.KioskID {
		return fmt.Errorf("update job progress: kiosk ID mismatch: event has %q, order belongs to %q", event.KioskID, orderKioskID)
	}

	// 3. Verify progress bounds
	if event.PagesPrinted > totalPages {
		return fmt.Errorf("update job progress: pages_printed (%d) exceeds order total_pages (%d)", event.PagesPrinted, totalPages)
	}
	if event.SheetsPrinted > totalSheets {
		return fmt.Errorf("update job progress: sheets_printed (%d) exceeds order total_sheets (%d)", event.SheetsPrinted, totalSheets)
	}

	// 4. Check existing progress timestamp to reject older/stale events
	var existingTimestampNS int64
	err = tx.QueryRowContext(ctx, s.rebind("SELECT timestamp_ns FROM job_progress WHERE job_id = ?;"), event.JobID).Scan(&existingTimestampNS)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("update job progress: query existing progress: %w", err)
	}
	if err == nil && eventNS < existingTimestampNS {
		return fmt.Errorf("update job progress: rejected stale event timestamp (%d < %d)", eventNS, existingTimestampNS)
	}

	// 5. Upsert latest progress row
	upsertProgress := `
INSERT INTO job_progress (job_id, kiosk_id, status, pages_printed, sheets_printed, error_reason, timestamp_ns)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(job_id) DO UPDATE SET
    kiosk_id = excluded.kiosk_id,
    status = excluded.status,
    pages_printed = excluded.pages_printed,
    sheets_printed = excluded.sheets_printed,
    error_reason = excluded.error_reason,
    timestamp_ns = excluded.timestamp_ns;
`
	_, err = tx.ExecContext(ctx, s.rebind(upsertProgress),
		event.JobID,
		event.KioskID,
		string(event.Status),
		event.PagesPrinted,
		event.SheetsPrinted,
		event.ErrorReason,
		eventNS,
	)
	if err != nil {
		return fmt.Errorf("update job progress: upsert progress: %w", err)
	}

	// 6. Update associated order's status and updated_at_ns
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE job_id = ?;
`), string(event.Status), eventNS, event.JobID)
	if err != nil {
		return fmt.Errorf("update job progress: update order status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update job progress: commit tx: %w", err)
	}

	return nil
}

// GetJobProgress retrieves the latest recorded progress event for a job.
func (s *Store) GetJobProgress(ctx context.Context, jobID string) (*contracts.JobProgressEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get job progress: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get job progress: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get job progress: %w", err)
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, fmt.Errorf("get job progress: jobID cannot be empty")
	}

	query := `
SELECT job_id, kiosk_id, status, pages_printed, sheets_printed, error_reason, timestamp_ns
FROM job_progress
WHERE job_id = ?;
`
	var (
		resJobID      string
		kioskID       string
		statusStr     string
		pagesPrinted  int
		sheetsPrinted int
		errorReason   string
		timestampNS   int64
	)

	err := s.db.QueryRowContext(ctx, s.rebind(query), jobID).Scan(
		&resJobID,
		&kioskID,
		&statusStr,
		&pagesPrinted,
		&sheetsPrinted,
		&errorReason,
		&timestampNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get job progress: %w", err)
	}

	event := &contracts.JobProgressEvent{
		JobID:         resJobID,
		KioskID:       kioskID,
		Status:        contracts.JobStatus(statusStr),
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		ErrorReason:   errorReason,
		Timestamp:     time.Unix(0, timestampNS).UTC(),
	}

	return event, nil
}

func isValidJobStatus(s contracts.JobStatus) bool {
	switch s {
	case contracts.JobStatusUploaded,
		contracts.JobStatusNormalized,
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusCompleted,
		contracts.JobStatusPausedError,
		contracts.JobStatusFailedRefundable:
		return true
	default:
		return false
	}
}

func isValidDuplex(d contracts.DuplexMode) bool {
	switch d {
	case contracts.DuplexSimplex,
		contracts.DuplexLongEdge,
		contracts.DuplexShortEdge:
		return true
	default:
		return false
	}
}
