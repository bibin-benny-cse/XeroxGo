-- XeroxGo Supabase PostgreSQL Schema
-- Idempotent table definitions for orders, print configurations, pricing, and job progress.

CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL UNIQUE,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    order_secret_hash TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS print_configs (
    order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE,
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
    price_per_sheet_paise BIGINT NOT NULL CHECK (price_per_sheet_paise > 0),
    bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200 CHECK (bw_price_per_sheet_paise > 0),
    color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000 CHECK (color_price_per_sheet_paise > 0),
    bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200 CHECK (bw_simplex_price_paise > 0),
    bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150 CHECK (bw_duplex_price_paise > 0),
    color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000 CHECK (color_simplex_price_paise > 0),
    color_duplex_price_paise BIGINT NOT NULL DEFAULT 750 CHECK (color_duplex_price_paise > 0),
    color_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_webhooks (
    webhook_id TEXT PRIMARY KEY,
    received_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_progress (
    job_id TEXT PRIMARY KEY REFERENCES orders(job_id) ON DELETE CASCADE,
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

-- Registered Kiosks and independent machine credentials
CREATE TABLE IF NOT EXISTS kiosks (
    kiosk_id TEXT PRIMARY KEY,
    secret_hash TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

-- Four-Digit Customer Pickup PINs (stores only HMAC-SHA256, never plaintext PIN)
CREATE TABLE IF NOT EXISTS pickup_pins (
    id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    pin_hmac TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL,
    redeemed_at_ns BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pickup_pins_active_kiosk_hmac
    ON pickup_pins(kiosk_id, pin_hmac)
    WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_pickup_pins_order ON pickup_pins(order_id);
CREATE INDEX IF NOT EXISTS idx_pickup_pins_expires ON pickup_pins(expires_at_ns);
CREATE INDEX IF NOT EXISTS idx_pickup_pins_kiosk_status ON pickup_pins(kiosk_id, status);

-- Payment Sessions (binds machine session to order upon successful PIN redemption)
-- Protected against cascading deletion (ON DELETE RESTRICT)
CREATE TABLE IF NOT EXISTS payment_sessions (
    session_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE RESTRICT,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_sessions_order ON payment_sessions(order_id);
CREATE INDEX IF NOT EXISTS idx_payment_sessions_kiosk ON payment_sessions(kiosk_id);

-- Payment Attempts & Audit History
-- Protected against cascading deletion (ON DELETE RESTRICT)
CREATE TABLE IF NOT EXISTS payment_attempts (
    id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE RESTRICT,
    session_id TEXT NOT NULL REFERENCES payment_sessions(session_id) ON DELETE RESTRICT,
    kiosk_id TEXT NOT NULL,
    payment_method TEXT NOT NULL,
    razorpay_qr_id TEXT NOT NULL DEFAULT '',
    razorpay_order_id TEXT NOT NULL DEFAULT '',
    razorpay_payment_id TEXT NOT NULL DEFAULT '',
    qr_image_url TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    currency TEXT NOT NULL DEFAULT 'INR',
    status TEXT NOT NULL DEFAULT 'CREATED',
    error_reason TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_attempts_qr_id
    ON payment_attempts(razorpay_qr_id)
    WHERE razorpay_qr_id != '';
CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts(order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts(session_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_payment_id ON payment_attempts(razorpay_payment_id);

-- Payment Webhook Events for Razorpay Idempotency and Deduplication
CREATE TABLE IF NOT EXISTS payment_webhook_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    payload_hash TEXT NOT NULL DEFAULT '',
    order_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'RECEIVED',
    received_at_ns BIGINT NOT NULL,
    processed_at_ns BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_payment_webhook_events_order ON payment_webhook_events(order_id);
