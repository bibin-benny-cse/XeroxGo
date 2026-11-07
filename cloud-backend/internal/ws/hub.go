package ws

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

type wireMessage struct {
	Type     string                      `json:"type"`
	Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health   *contracts.PrinterHealth    `json:"health,omitempty"`
	Ads      []contracts.AdItem          `json:"ads,omitempty"`
	Error    string                      `json:"error,omitempty"`
}

type kioskSession struct {
	kioskID string
	conn    *websocket.Conn
	sendMu  sync.Mutex
	closed  chan struct{}
}

// Hub manages active WebSocket connections from kiosks and relays jobs,
// progress events, and printer health observations.
type Hub struct {
	db       *store.Store
	mu       sync.RWMutex
	sessions map[string]*kioskSession

	healthMu sync.RWMutex
	health   map[string]contracts.PrinterHealth
}

// NewHub creates and initializes a new cloud WebSocket hub.
func NewHub(db *store.Store) *Hub {
	return &Hub{
		db:       db,
		sessions: make(map[string]*kioskSession),
		health:   make(map[string]contracts.PrinterHealth),
	}
}

// ServeHTTP handles incoming WebSocket connection upgrades at /ws?kiosk_id=<id>.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	kioskID := strings.TrimSpace(r.URL.Query().Get("kiosk_id"))
	if kioskID == "" {
		http.Error(w, "kiosk_id required", http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	conn.SetReadLimit(1024 * 1024)

	sess := &kioskSession{
		kioskID: kioskID,
		conn:    conn,
		closed:  make(chan struct{}),
	}

	h.mu.Lock()
	if old, exists := h.sessions[kioskID]; exists {
		_ = old.conn.Close(websocket.StatusPolicyViolation, "replaced by new connection")
	}
	h.sessions[kioskID] = sess
	h.mu.Unlock()
	log.Printf("[HUB] Kiosk connected: %q (client IP: %s)", kioskID, r.RemoteAddr)

	defer func() {
		h.mu.Lock()
		if h.sessions[kioskID] == sess {
			delete(h.sessions, kioskID)
		}
		h.mu.Unlock()
		close(sess.closed)
		log.Printf("[HUB] Kiosk disconnected: %q", kioskID)
		_ = conn.Close(websocket.StatusNormalClosure, "session ended")
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Proactively sync active ads to newly connected kiosk
	if h.db != nil {
		if activeAds, err := h.db.GetActiveAds(ctx); err == nil && len(activeAds) > 0 {
			_ = wsjson.Write(ctx, conn, wireMessage{Type: "sync_ads", Ads: activeAds})
		}
	}

	for {
		var msg wireMessage
		err := wsjson.Read(ctx, conn, &msg)
		if err != nil {
			break
		}

		switch msg.Type {
		case "heartbeat":
			// Maintain liveness
		case "progress":
			if msg.Progress == nil {
				continue
			}
			if msg.Progress.KioskID != kioskID {
				// Reject mismatched kiosk progress
				continue
			}
			if strings.TrimSpace(msg.Progress.JobID) == "" || !isValidJobStatus(msg.Progress.Status) || msg.Progress.Timestamp.IsZero() {
				continue
			}
			if h.db != nil {
				_ = h.db.UpdateJobProgress(ctx, *msg.Progress)
			}
		case "health":
			if msg.Health == nil {
				continue
			}
			if !isValidHealthState(msg.Health.State) || !isValidTrayState(msg.Health.PaperTrayStatus) {
				continue
			}
			if msg.Health.TonerLevelPercent < 0 || msg.Health.TonerLevelPercent > 100 ||
				msg.Health.DrumLifePercent < 0 || msg.Health.DrumLifePercent > 100 {
				continue
			}
			h.healthMu.Lock()
			h.health[kioskID] = *msg.Health
			h.healthMu.Unlock()
		case "error":
			// Operational error
		default:
			// Ignore unexpected type
		}
	}
}

// SendJob dispatches a PrintJobCommand envelope to the connected kiosk.
func (h *Hub) SendJob(ctx context.Context, cmd contracts.PrintJobCommand) error {
	if h == nil {
		return fmt.Errorf("send job: hub is nil")
	}
	if ctx == nil {
		return fmt.Errorf("send job: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send job: %w", err)
	}

	if strings.TrimSpace(cmd.JobID) == "" {
		return fmt.Errorf("send job: empty job ID")
	}
	if strings.TrimSpace(cmd.KioskID) == "" {
		return fmt.Errorf("send job: empty kiosk ID")
	}
	if strings.TrimSpace(cmd.DownloadURL) == "" {
		return fmt.Errorf("send job: empty download URL")
	}
	if cmd.TotalPages < 1 {
		return fmt.Errorf("send job: total pages must be >= 1")
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("send job: total sheets must be >= 1")
	}
	if cmd.Copies < 1 {
		return fmt.Errorf("send job: copies must be >= 1")
	}
	if !isValidDuplex(cmd.Duplex) {
		return fmt.Errorf("send job: invalid duplex mode %q", cmd.Duplex)
	}
	if cmd.StartFromPage < 1 || cmd.StartFromPage > cmd.TotalPages {
		return fmt.Errorf("send job: invalid start from page %d", cmd.StartFromPage)
	}

	h.mu.RLock()
	sess, ok := h.sessions[cmd.KioskID]
	h.mu.RUnlock()

	if !ok || sess == nil {
		return fmt.Errorf("send job: kiosk %q not connected", cmd.KioskID)
	}

	msg := wireMessage{
		Type: "job",
		Job:  &cmd,
	}

	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()

	if err := wsjson.Write(ctx, sess.conn, msg); err != nil {
		return fmt.Errorf("send job: write to kiosk %q: %w", cmd.KioskID, err)
	}

	return nil
}

// IsConnected returns whether the specified kiosk currently has an active WebSocket session.
func (h *Hub) IsConnected(kioskID string) bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	sess, ok := h.sessions[kioskID]
	return ok && sess != nil
}

// LatestHealth returns the most recent cached printer health observation for a kiosk.
func (h *Hub) LatestHealth(kioskID string) (contracts.PrinterHealth, bool) {
	if h == nil {
		return contracts.PrinterHealth{}, false
	}
	h.healthMu.RLock()
	defer h.healthMu.RUnlock()

	health, ok := h.health[kioskID]
	return health, ok
}

func isValidHealthState(state string) bool {
	switch state {
	case "IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE":
		return true
	default:
		return false
	}
}

func isValidTrayState(tray string) bool {
	switch tray {
	case "OK", "LOW", "EMPTY":
		return true
	default:
		return false
	}
}

func isValidDuplex(d contracts.DuplexMode) bool {
	switch d {
	case contracts.DuplexSimplex, contracts.DuplexLongEdge, contracts.DuplexShortEdge:
		return true
	default:
		return false
	}
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

// BroadcastSyncAds broadcasts updated ads to all currently connected kiosks in real time.
func (h *Hub) BroadcastSyncAds(ctx context.Context, ads []contracts.AdItem) {
	h.mu.RLock()
	sessions := make([]*kioskSession, 0, len(h.sessions))
	for _, sess := range h.sessions {
		sessions = append(sessions, sess)
	}
	h.mu.RUnlock()

	msg := wireMessage{Type: "sync_ads", Ads: ads}
	for _, sess := range sessions {
		sess.sendMu.Lock()
		_ = wsjson.Write(ctx, sess.conn, msg)
		sess.sendMu.Unlock()
	}
}

// ConnectedKiosksCount returns the number of currently connected kiosks.
func (h *Hub) ConnectedKiosksCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}
