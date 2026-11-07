package contracts

import "time"

type JobStatus string

const (
	JobStatusUploaded         JobStatus = "UPLOADED"
	JobStatusNormalized       JobStatus = "NORMALIZED"
	JobStatusPendingPayment   JobStatus = "PENDING_PAYMENT"
	JobStatusPaidWaiting      JobStatus = "PAID_WAITING"
	JobStatusDispatched       JobStatus = "DISPATCHED"
	JobStatusSpooling         JobStatus = "SPOOLING"
	JobStatusPrinting         JobStatus = "PRINTING"
	JobStatusCompleted        JobStatus = "COMPLETED"
	JobStatusPausedError      JobStatus = "PAUSED_ERROR"
	JobStatusFailedRefundable JobStatus = "FAILED_REFUNDABLE"
)

type DuplexMode string

const (
	DuplexSimplex   DuplexMode = "SIMPLEX"
	DuplexLongEdge  DuplexMode = "DUPLEX_LONG_EDGE"
	DuplexShortEdge DuplexMode = "DUPLEX_SHORT_EDGE"
)

type PrinterHealth struct {
	Online            bool      `json:"online"`
	State             string    `json:"state"`               // "IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE"
	PaperTrayStatus   string    `json:"paper_tray_status"`   // "OK", "LOW", "EMPTY"
	EstimatedSheets   int       `json:"estimated_sheets"`    // Tracked remaining sheets in 250-sheet tray
	TonerLevelPercent int       `json:"toner_level_percent"` // 0 - 100
	DrumLifePercent   int       `json:"drum_life_percent"`   // 0 - 100
	CoverOpen         bool      `json:"cover_open"`
	PaperJam          bool      `json:"paper_jam"`
	OutputBinFull     bool      `json:"output_bin_full"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type PrintJobCommand struct {
	JobID          string     `json:"job_id"`
	KioskID        string     `json:"kiosk_id"`
	DisplayCode    string     `json:"display_code"`    // 4-char code shown on HDMI screen (e.g. "A74X")
	DownloadURL    string     `json:"download_url"`    // Signed URL to download normalized PDF
	SHA256Checksum string     `json:"sha256_checksum"` // Verified by Pi before printing
	TotalPages     int        `json:"total_pages"`     // Logical PDF pages after normalization
	TotalSheets    int        `json:"total_sheets"`    // Physical A4 sheets required
	Copies         int        `json:"copies"`
	Duplex         DuplexMode `json:"duplex"`
	StartFromPage  int        `json:"start_from_page"` // 1 normally; >1 when resuming after crash/paper out
}

type JobProgressEvent struct {
	JobID         string    `json:"job_id"`
	KioskID       string    `json:"kiosk_id"`
	Status        JobStatus `json:"status"`
	PagesPrinted  int       `json:"pages_printed"`
	SheetsPrinted int       `json:"sheets_printed"`
	ErrorReason   string    `json:"error_reason,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}
