package ui_ipc

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	_ "golang.org/x/image/webp"
	"xeroxgo/pkg/contracts"
)

const (
	ScreenWidth  = 1920
	ScreenHeight = 1080
)

type ScreenMode string

const (
	ModeIdle              ScreenMode = "IDLE"
	ModePINEntry          ScreenMode = "PIN_ENTRY"
	ModePaymentQR         ScreenMode = "PAYMENT_QR"
	ModeWaitingForPayment ScreenMode = "WAITING_FOR_PAYMENT"
	ModePaymentFailed     ScreenMode = "PAYMENT_FAILED"
	ModeDownloading       ScreenMode = "DOWNLOADING"
	ModePrinting          ScreenMode = "PRINTING"
	ModeCompleted         ScreenMode = "COMPLETED"
	ModeError             ScreenMode = "ERROR"
)

type ScreenState struct {
	Mode            ScreenMode
	QRContent       string
	Message         string
	Detail          string
	ProgressPercent int
	CurrentPage     int
	TotalPages      int
	PrinterModel    string
	AdSlideIndex    int     // 0: Quick & Easy, 1: High Quality Prints, 2: Skip The Queue
	TransitionFrom  int     // -1 if static; 0, 1, or 2 if transitioning from that slide
	TransitionProg  float64 // 0.0 to 1.0 progress of slide transition

	// Pickup PIN and Payment workflow fields (Stage 4)
	PIN              string      // 4-digit numeric PIN, preserving leading zeros (e.g. "0482")
	PINError         string      // Validation/rate-limit error message on PIN entry screen
	OrderID          string      // Order identifier (e.g. "ord_abc123")
	AmountPaise      int64       // Payable amount in paise (e.g. 500 = ₹5.00)
	PaymentQRURL     string      // Razorpay UPI QR code URL or UPI URI string
	PaymentQRImage   image.Image // Pre-decoded or downloaded QR image
	PaymentExpiresAt time.Time   // Expiration timestamp for payment countdown
	PaymentStatus    string      // e.g. "Awaiting payment verification..."
	PaymentError     string      // Reason for payment failure or timeout
	CanRetryPayment  bool        // True if the active payment session can be retried without PIN re-entry
}

type Sink func(*image.RGBA) error

var (
	// Frontend portal.css aligned palette
	colorBgSlate          = color.RGBA{R: 248, G: 250, B: 252, A: 255} // #F8FAFC
	colorCardBg           = color.RGBA{R: 255, G: 255, B: 255, A: 255} // #FFFFFF
	colorBorder           = color.RGBA{R: 226, G: 232, B: 240, A: 255} // #E2E8F0
	colorBorderDark       = color.RGBA{R: 203, G: 213, B: 225, A: 255} // #CBD5E1
	colorPrimaryBlue      = color.RGBA{R: 37, G: 99, B: 235, A: 255}   // #2563EB
	colorAccentBlue       = colorPrimaryBlue                           // Alias for tests
	colorSuccessGreen     = color.RGBA{R: 16, G: 185, B: 129, A: 255}  // #10B981
	colorWarningAmber     = color.RGBA{R: 245, G: 158, B: 11, A: 255}  // #F59E0B
	colorErrorRed         = color.RGBA{R: 239, G: 68, B: 68, A: 255}   // #EF4444
	colorTextPrimary      = color.RGBA{R: 30, G: 41, B: 59, A: 255}    // #1E293B
	colorTextMutedDark    = color.RGBA{R: 51, G: 65, B: 85, A: 255}    // #334155 (Slate-700, dark gray)
	colorTextSecondary    = color.RGBA{R: 100, G: 116, B: 139, A: 255} // #64748B
	colorTextWhite        = color.RGBA{R: 255, G: 255, B: 255, A: 255} // #FFFFFF
	colorBarBg            = color.RGBA{R: 226, G: 232, B: 240, A: 255} // #E2E8F0
	colorPillBlueBg       = color.RGBA{R: 239, G: 246, B: 255, A: 255} // #EFF6FF
	colorPillBlueBorder   = color.RGBA{R: 191, G: 219, B: 254, A: 255} // #BFDBFE
	colorPillGreenBg      = color.RGBA{R: 236, G: 253, B: 245, A: 255} // #ECFDF5
	colorPillGreenBorder  = color.RGBA{R: 167, G: 243, B: 208, A: 255} // #A7F3D0
	colorPillRedBg        = color.RGBA{R: 254, G: 242, B: 242, A: 255} // #FEF2F2
	colorPillRedBorder    = color.RGBA{R: 254, G: 202, B: 202, A: 255} // #FECACA
	colorPillAmberBg      = color.RGBA{R: 254, G: 243, B: 199, A: 255} // #FEF3C7
	colorWarningAmberDark = color.RGBA{R: 180, G: 83, B: 9, A: 255}    // #B45309
)

var (
	devModeMu sync.RWMutex
	isDevMode bool
)

// SetDevelopmentMode enables or disables the development mode indicator banner across all kiosk screens.
func SetDevelopmentMode(enabled bool) {
	devModeMu.Lock()
	defer devModeMu.Unlock()
	isDevMode = enabled
}

// IsDevelopmentMode returns whether development mode is active.
func IsDevelopmentMode() bool {
	devModeMu.RLock()
	defer devModeMu.RUnlock()
	return isDevMode
}

func drawDevBadge(dst *image.RGBA) {
	badge := image.Rect(1530, 32, 1860, 78)
	drawPill(dst, badge, colorPillAmberBg, colorWarningAmber)
	text := "DEV MODE (WINDOWS)"
	w := measureTextWidth(faceBold18, text)
	drawText(dst, faceBold18, text, badge.Min.X+(badge.Dx()-w)/2, badge.Min.Y+30, colorWarningAmberDark)
}

var renderMu sync.Mutex

// Validate checks that the ScreenState has valid mode and range values.
func (state ScreenState) Validate() error {
	switch state.Mode {
	case ModeIdle:
		if strings.TrimSpace(state.QRContent) == "" {
			return fmt.Errorf("idle mode: QRContent cannot be empty or whitespace-only")
		}
	case ModePINEntry:
		if len(state.PIN) > 4 {
			return fmt.Errorf("pin entry mode: PIN length cannot exceed 4 digits, got %q", state.PIN)
		}
	case ModePaymentQR:
		if state.AmountPaise < 0 {
			return fmt.Errorf("payment qr mode: AmountPaise cannot be negative, got %d", state.AmountPaise)
		}
	case ModeWaitingForPayment:
		if state.AmountPaise < 0 {
			return fmt.Errorf("waiting for payment mode: AmountPaise cannot be negative, got %d", state.AmountPaise)
		}
	case ModePaymentFailed:
		// Valid
	case ModeDownloading:
		if state.ProgressPercent < 0 || state.ProgressPercent > 100 {
			return fmt.Errorf("downloading mode: ProgressPercent must be between 0 and 100, got %d", state.ProgressPercent)
		}
	case ModePrinting:
		if state.TotalPages < 1 {
			return fmt.Errorf("printing mode: TotalPages must be >= 1, got %d", state.TotalPages)
		}
		if state.CurrentPage < 0 || state.CurrentPage > state.TotalPages {
			return fmt.Errorf("printing mode: CurrentPage must be between 0 and TotalPages (%d), got %d", state.TotalPages, state.CurrentPage)
		}
	case ModeCompleted:
		// Valid
	case ModeError:
		// Valid
	default:
		return fmt.Errorf("unsupported screen mode: %q", state.Mode)
	}
	return nil
}

// RenderTo renders the screen state directly into an existing destination buffer.
// This zero-alloc path enables 60 FPS transitions without triggering Go GC passes.
func RenderTo(dst *image.RGBA, state ScreenState) error {
	if dst == nil {
		return fmt.Errorf("destination buffer cannot be nil")
	}
	if err := state.Validate(); err != nil {
		return fmt.Errorf("invalid screen state: %w", err)
	}

	renderMu.Lock()
	defer renderMu.Unlock()

	initUIAssets()

	// Fast-path: Idle mode renders directly into dst (native or scaled)
	if state.Mode == ModeIdle {
		return renderIdle(dst, state)
	}

	// For non-idle modes (Downloading, Printing, Completed, Error):
	var fullCanvas *image.RGBA
	if dst.Bounds().Dx() == ScreenWidth && dst.Bounds().Dy() == ScreenHeight {
		fullCanvas = dst
	} else {
		fullCanvas = image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	}

	// 1. Base clean Slate-50 background matching portal.css
	fillRect(fullCanvas, fullCanvas.Bounds(), colorBgSlate)

	// 2. Top white header bar (0 to 110)
	fillRect(fullCanvas, image.Rect(0, 0, ScreenWidth, 110), colorCardBg)
	fillRect(fullCanvas, image.Rect(0, 108, ScreenWidth, 110), colorBorder)

	// Embedded XeroxGo full logo
	drawLogo(fullCanvas, 80, 31, 48)

	// Subtitle beside brand logo
	drawText(fullCanvas, faceReg22, "Upload, Print, Collect - It's that simple!", 480, 44, colorTextSecondary)

	if IsDevelopmentMode() {
		drawDevBadge(fullCanvas)
	}

	// 4. Mode-specific content
	switch state.Mode {
	case ModePINEntry:
		renderPINEntry(fullCanvas, state)
	case ModePaymentQR:
		renderPaymentQR(fullCanvas, state)
	case ModeWaitingForPayment:
		renderWaitingForPayment(fullCanvas, state)
	case ModePaymentFailed:
		renderPaymentFailed(fullCanvas, state)
	case ModeDownloading:
		renderDownloading(fullCanvas, state)
	case ModePrinting:
		renderPrinting(fullCanvas, state)
	case ModeCompleted:
		renderCompleted(fullCanvas, state)
	case ModeError:
		renderError(fullCanvas, state)
	}

	if fullCanvas != dst {
		xdraw.BiLinear.Scale(dst, dst.Bounds(), fullCanvas, fullCanvas.Bounds(), xdraw.Over, nil)
	}

	return nil
}

// Render renders a dashboard image for the given state.
// Preserves full backward-compatibility for callers and unit tests.
func Render(state ScreenState) (*image.RGBA, error) {
	targetW, targetH := GetTargetResolution()
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	if err := RenderTo(dst, state); err != nil {
		return nil, err
	}
	return dst, nil
}

// AdSlide models a single promotional slide from the mobile frontend carousel.
type AdSlide struct {
	Title string
	Desc  string
	C0    color.RGBA
	C1    color.RGBA
	C2    color.RGBA
}

var defaultAdItems = []contracts.AdItem{
	{
		ID:          "default-1",
		Title:       "Quick & Easy",
		Description: "Direct UPI payment. No coins, no cash needed.",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#1E3A8A",
		GradientC1:  "#2563EB",
		GradientC2:  "#4F46E5",
		SortOrder:   1,
		IsActive:    true,
	},
	{
		ID:          "default-2",
		Title:       "High Quality Prints",
		Description: "Crisp laser documents ready in seconds.",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#065F46",
		GradientC1:  "#10B981",
		GradientC2:  "#059669",
		SortOrder:   2,
		IsActive:    true,
	},
	{
		ID:          "default-3",
		Title:       "Skip The Queue",
		Description: "Pick up your print job with your 4-digit code.",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#581C87",
		GradientC1:  "#7C3AED",
		GradientC2:  "#D97706",
		SortOrder:   3,
		IsActive:    true,
	},
}

var (
	targetGeomMu sync.RWMutex
	targetGeom   = FramebufferGeometry{
		Width:        ScreenWidth,
		Height:       ScreenHeight,
		BitsPerPixel: 32,
		Stride:       ScreenWidth * 4,
	}

	adSlidesMu           sync.RWMutex
	adSlideImgs          []*image.RGBA
	scaledAdSlideImgs    []*image.RGBA
	cachedScaledIdleBase *image.RGBA
	cachedScaledIdleQR   string

	cachedQRContent string
	cachedQRImg     image.Image
	qrCacheMu       sync.Mutex

	cachedIdleBase *image.RGBA
	cachedIdleQR   string
	idleBaseMu     sync.Mutex
)

// GetTargetResolution returns the currently configured display width and height.
func GetTargetResolution() (int, int) {
	targetGeomMu.RLock()
	defer targetGeomMu.RUnlock()
	return targetGeom.Width, targetGeom.Height
}

// ConfigureTarget configures the target display resolution and pre-scales static
// bases and ad slides once at startup, guaranteeing 0 ms downscaling and 0 heap
// allocations during 60 FPS carousel animation loops on Raspberry Pi hardware.
func ConfigureTarget(geom FramebufferGeometry) {
	targetGeomMu.Lock()
	defer targetGeomMu.Unlock()

	if geom.Width <= 0 {
		geom.Width = ScreenWidth
	}
	if geom.Height <= 0 {
		geom.Height = ScreenHeight
	}
	if geom.BitsPerPixel <= 0 {
		geom.BitsPerPixel = 32
	}
	if geom.Stride <= 0 {
		geom.Stride = geom.Width * (geom.BitsPerPixel / 8)
	}

	targetGeom = geom

	idleBaseMu.Lock()
	cachedScaledIdleBase = nil
	cachedScaledIdleQR = ""
	idleBaseMu.Unlock()

	adSlidesMu.Lock()
	scaledAdSlideImgs = nil
	adSlidesMu.Unlock()

	if geom.Width != ScreenWidth || geom.Height != ScreenHeight {
		initScaledAdSlides(geom.Width, geom.Height)
	}
}

// initScaledAdSlides pre-scales all promotional slides into memory buffers once for the target resolution.
func initScaledAdSlides(targetW, targetH int) {
	sx := float64(targetW) / float64(ScreenWidth)
	sy := float64(targetH) / float64(ScreenHeight)

	cardW := int(float64(1203)*sx + 0.5)
	cardH := int(float64(840)*sy + 0.5)
	if cardW < 1 {
		cardW = 1
	}
	if cardH < 1 {
		cardH = 1
	}

	adSlidesMu.Lock()
	defer adSlidesMu.Unlock()

	scaled := make([]*image.RGBA, len(adSlideImgs))
	for k := range adSlideImgs {
		img := image.NewRGBA(image.Rect(0, 0, cardW, cardH))
		xdraw.BiLinear.Scale(img, img.Bounds(), adSlideImgs[k], adSlideImgs[k].Bounds(), xdraw.Over, nil)
		scaled[k] = img
	}
	scaledAdSlideImgs = scaled
}

// GetAdSlideCount returns the number of active promotional ad slides.
func GetAdSlideCount() int {
	initUIAssets()
	adSlidesMu.RLock()
	defer adSlidesMu.RUnlock()
	if len(adSlideImgs) == 0 {
		return 3
	}
	return len(adSlideImgs)
}

// GetAdSlideImage returns the pre-rendered promotional ad card image for previewing or inspection.
func GetAdSlideImage(idx int) *image.RGBA {
	initUIAssets()
	adSlidesMu.RLock()
	defer adSlidesMu.RUnlock()
	if len(adSlideImgs) == 0 || idx < 0 || idx >= len(adSlideImgs) {
		return nil
	}
	return adSlideImgs[idx]
}

// ConfigureAds updates the promotional ad slides dynamically in real-time.
// Pre-renders 1203x840 memory buffers for each active ad slide, ensuring zero
// CPU or memory allocation overhead during 60 FPS idle animation loops.
func ConfigureAds(items []contracts.AdItem) {
	initUIAssets()
	configureAdsInternal(items)
}

func configureAdsInternal(items []contracts.AdItem) {
	var active []contracts.AdItem
	for _, it := range items {
		if it.IsActive {
			active = append(active, it)
		}
	}
	if len(active) == 0 {
		active = defaultAdItems
	}

	newImgs := make([]*image.RGBA, len(active))
	for i, it := range active {
		newImgs[i] = renderSingleAdSlide(it)
	}

	targetGeomMu.RLock()
	geom := targetGeom
	targetGeomMu.RUnlock()

	var newScaled []*image.RGBA
	if geom.Width != ScreenWidth || geom.Height != ScreenHeight {
		sx := float64(geom.Width) / float64(ScreenWidth)
		sy := float64(geom.Height) / float64(ScreenHeight)
		cardW := int(float64(1220)*sx + 0.5)
		cardH := int(float64(910)*sy + 0.5)
		if cardW < 1 {
			cardW = 1
		}
		if cardH < 1 {
			cardH = 1
		}
		newScaled = make([]*image.RGBA, len(newImgs))
		for k := range newImgs {
			scaled := image.NewRGBA(image.Rect(0, 0, cardW, cardH))
			xdraw.BiLinear.Scale(scaled, scaled.Bounds(), newImgs[k], newImgs[k].Bounds(), xdraw.Over, nil)
			newScaled[k] = scaled
		}
	}

	adSlidesMu.Lock()
	adSlideImgs = newImgs
	scaledAdSlideImgs = newScaled
	adSlidesMu.Unlock()
}

func renderSingleAdSlide(ad contracts.AdItem) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1220, 910))

	if ad.Type == contracts.AdTypeImage && ad.ImageURL != "" {
		if strings.HasPrefix(ad.ImageURL, "http://") || strings.HasPrefix(ad.ImageURL, "https://") {
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(ad.ImageURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				decoded, _, err := image.Decode(resp.Body)
				_ = resp.Body.Close()
				if err == nil {
					xdraw.BiLinear.Scale(img, img.Bounds(), decoded, decoded.Bounds(), xdraw.Over, nil)
					return img
				}
			}
		}
	}

	// Dynamic 135-degree 3-stop Hermite gradient matching mobile portal
	c0 := parseHexColorDefault(ad.GradientC0, color.RGBA{R: 30, G: 58, B: 138, A: 255})
	c1 := parseHexColorDefault(ad.GradientC1, color.RGBA{R: 37, G: 99, B: 235, A: 255})
	c2 := parseHexColorDefault(ad.GradientC2, color.RGBA{R: 79, G: 70, B: 229, A: 255})
	fillLinearGradient135(img, img.Bounds(), c0, c1, c2)

	startX := 80
	if ad.Title != "" {
		drawText(img, faceBold56, ad.Title, startX, 310, colorTextWhite)
	}
	if ad.Description != "" {
		drawText(img, faceReg28, ad.Description, startX, 396, color.RGBA{R: 226, G: 232, B: 240, A: 255})
	}
	return img
}

func parseHexColorDefault(hexStr string, def color.RGBA) color.RGBA {
	s := strings.TrimPrefix(hexStr, "#")
	if len(s) != 6 {
		return def
	}
	rgb, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return def
	}
	return color.RGBA{
		R: uint8(rgb >> 16),
		G: uint8(rgb >> 8),
		B: uint8(rgb),
		A: 255,
	}
}

// initAdSlides pre-renders promotional slides into memory buffers once at startup.
func initAdSlides() {
	if len(adSlideImgs) == 0 {
		configureAdsInternal(defaultAdItems)
	}
}

// GetQRCodeImage encodes a string into an in-memory QR code image with caching.
func GetQRCodeImage(content string) (image.Image, error) {
	qrCacheMu.Lock()
	defer qrCacheMu.Unlock()
	if content == cachedQRContent && cachedQRImg != nil {
		return cachedQRImg, nil
	}
	qrBytes, err := qrcode.Encode(content, qrcode.Medium, 400)
	if err != nil {
		return nil, fmt.Errorf("failed to encode QR code: %w", err)
	}
	img, err := png.Decode(bytes.NewReader(qrBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to decode QR PNG: %w", err)
	}
	cachedQRContent = content
	cachedQRImg = img
	return img, nil
}

func getQRCodeImage(content string) (image.Image, error) {
	return GetQRCodeImage(content)
}

// getIdleBase pre-renders the static elements of the Idle screen (header, right card, QR, steps, footer).
// Subsequent frames during the carousel transition copy this in ~0.25 ms with zero heap allocations.
func getIdleBase(qrContent string) (*image.RGBA, error) {
	idleBaseMu.Lock()
	defer idleBaseMu.Unlock()

	if cachedIdleBase != nil && cachedIdleQR == qrContent {
		return cachedIdleBase, nil
	}

	base := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))

	// 1. Base clean Slate-50 background matching portal.css
	fillRect(base, base.Bounds(), colorBgSlate)

	// 2. Top white header bar (0 to 110)
	fillRect(base, image.Rect(0, 0, ScreenWidth, 110), colorCardBg)
	fillRect(base, image.Rect(0, 108, ScreenWidth, 110), colorBorder)

	// Embedded XeroxGo full logo
	drawLogo(base, 80, 31, 48)

	// Subtitle beside brand logo
	drawText(base, faceReg22, "Upload, Print, Collect - It's that simple!", 480, 44, colorTextSecondary)

	if IsDevelopmentMode() {
		drawDevBadge(base)
	}

	// 4. Right Card: QR Code + Onboarding Steps
	rightCard := image.Rect(1280, 140, 1890, 1050)
	fillRoundedRect(base, rightCard, 24, colorCardBg)
	strokeRoundedRect(base, rightCard, 24, 2, colorBorder)

	// Headline above QR code - Option A (moved slightly down closer to QR)
	qrTitle := "Scan QR to Get Started"
	qrTitleW := measureTextWidth(faceBold28, qrTitle)
	drawText(base, faceBold28, qrTitle, rightCard.Min.X+(rightCard.Dx()-qrTitleW)/2, 164, colorTextPrimary)

	// Sub-caption below headline - Bold, slightly bigger (20px), dark gray (#334155)
	qrSub := "Point your phone camera here, or visit website below"
	qrSubW := measureTextWidth(faceBold20, qrSub)
	drawText(base, faceBold20, qrSub, rightCard.Min.X+(rightCard.Dx()-qrSubW)/2, 198, colorTextMutedDark)

	// QR Code Generation (Cached for zero-lag performance)
	qrImg, err := getQRCodeImage(qrContent)
	if err != nil {
		return nil, err
	}

	qrBounds := image.Rect(1405, 236, 1765, 596)
	draw.Draw(base, qrBounds, qrImg, image.Point{}, draw.Src)

	// Action pill under QR with website link
	pillRect := image.Rect(1355, 626, 1815, 682)
	drawPill(base, pillRect, colorPillBlueBg, colorPillBlueBorder)
	displayLink := "xeroxgo-frontend.vercel.app"
	if strings.TrimSpace(qrContent) != "" {
		cleaned := strings.TrimSpace(qrContent)
		if strings.HasPrefix(cleaned, "http://") || strings.HasPrefix(cleaned, "https://") {
			if u, err := url.Parse(cleaned); err == nil && u.Host != "" {
				displayLink = u.Host
			}
		} else if strings.Contains(cleaned, ".") && !strings.Contains(cleaned, " ") {
			displayLink = cleaned
		}
	}
	pillW := measureTextWidth(faceBold24, displayLink)
	drawText(base, faceBold24, displayLink, pillRect.Min.X+(pillRect.Dx()-pillW)/2, 642, colorPrimaryBlue)

	// 3 Numbered Steps - Option A (mathematically centered with 68px left/right margins)
	steps := []struct {
		num    string
		title  string
		isLast bool
	}{
		{"1", "Upload your file & choose print settings", false},
		{"2", "Pay with UPI (GPay, PhonePe, Paytm)", false},
		{"3", "Collect your prints from the tray below", true},
	}
	ySteps := []int{716, 784, 852}
	for i, s := range steps {
		y := ySteps[i]
		circleCol := colorPrimaryBlue
		if s.isLast {
			circleCol = colorSuccessGreen
		}
		drawCircle(base, 1366, y+18, 18, circleCol)
		numW := measureTextWidth(faceBold20, s.num)
		drawText(base, faceBold20, s.num, 1366-numW/2, y+9, colorTextWhite)

		drawText(base, faceBold22, s.title, 1398, y+7, colorTextPrimary)
	}

	// Security badge / footer text inside right card - Layman's terms & larger font (18px)
	secText := "Your files are private and automatically deleted after printing"
	secW := measureTextWidth(faceReg18, secText)
	drawText(base, faceReg18, secText, rightCard.Min.X+(rightCard.Dx()-secW)/2, 961, colorTextSecondary)

	cachedIdleBase = base
	cachedIdleQR = qrContent
	return base, nil
}

func renderIdle(dst *image.RGBA, state ScreenState) error {
	base, err := getIdleBase(state.QRContent)
	if err != nil {
		return err
	}

	adSlidesMu.RLock()
	defer adSlidesMu.RUnlock()

	numSlides := len(adSlideImgs)
	if numSlides == 0 {
		return nil
	}

	targetW, targetH := dst.Bounds().Dx(), dst.Bounds().Dy()
	isNative1080p := (targetW == ScreenWidth && targetH == ScreenHeight)

	var (
		cardW, cardH int
		adCard       image.Rectangle
		srcSlides    []*image.RGBA
		sx, sy       float64
	)

	if isNative1080p {
		copy(dst.Pix, base.Pix)
		adCard = image.Rect(30, 140, 1250, 1050)
		cardW = adCard.Dx()
		cardH = adCard.Dy()
		srcSlides = adSlideImgs
		sx = 1.0
		sy = 1.0
	} else {
		idleBaseMu.Lock()
		if cachedScaledIdleBase == nil || cachedScaledIdleQR != state.QRContent || cachedScaledIdleBase.Bounds().Dx() != targetW || cachedScaledIdleBase.Bounds().Dy() != targetH {
			scaled := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
			xdraw.BiLinear.Scale(scaled, scaled.Bounds(), base, base.Bounds(), xdraw.Over, nil)
			cachedScaledIdleBase = scaled
			cachedScaledIdleQR = state.QRContent
		}
		copy(dst.Pix, cachedScaledIdleBase.Pix)
		idleBaseMu.Unlock()

		sx = float64(targetW) / float64(ScreenWidth)
		sy = float64(targetH) / float64(ScreenHeight)

		cardMinX := int(30.0*sx + 0.5)
		cardMinY := int(140.0*sy + 0.5)
		cardMaxX := int(1250.0*sx + 0.5)
		cardMaxY := int(1050.0*sy + 0.5)
		adCard = image.Rect(cardMinX, cardMinY, cardMaxX, cardMaxY)
		cardW = adCard.Dx()
		cardH = adCard.Dy()

		srcSlides = scaledAdSlideImgs
		if len(srcSlides) != numSlides || srcSlides[0] == nil || srcSlides[0].Bounds().Dx() != cardW || srcSlides[0].Bounds().Dy() != cardH {
			srcSlides = adSlideImgs
		}
	}

	toIdx := (state.AdSlideIndex%numSlides + numSlides) % numSlides
	isTransition := (numSlides > 1) &&
		state.TransitionFrom >= 0 &&
		state.TransitionFrom != state.AdSlideIndex &&
		state.TransitionProg > 0.0 &&
		state.TransitionProg < 1.0

	var fromIdx int
	var offset int
	var easeProg float64

	if isTransition {
		fromIdx = (state.TransitionFrom%numSlides + numSlides) % numSlides
		p := state.TransitionProg
		if p < 0.0 {
			p = 0.0
		} else if p > 1.0 {
			p = 1.0
		}
		// Cubic Hermite smoothstep ease-in-out: e = p * p * (3 - 2 * p)
		easeProg = p * p * (3.0 - 2.0*p)
		offset = int(easeProg*float64(cardW) + 0.5)
		if offset < 0 {
			offset = 0
		} else if offset > cardW {
			offset = cardW
		}
	} else {
		fromIdx = toIdx
		offset = 0
		easeProg = 0.0
	}

	wFrom := cardW - offset
	wTo := offset

	// Blit horizontal sliding track row-by-row with assembly-optimized byte slice copies
	for y := 0; y < cardH; y++ {
		dstY := adCard.Min.Y + y
		dstRowStart := dstY*dst.Stride + adCard.Min.X*4
		dstRow := dst.Pix[dstRowStart : dstRowStart+cardW*4]
		srcYStride := y * srcSlides[0].Stride

		if wFrom > 0 {
			srcFrom := srcSlides[fromIdx].Pix[srcYStride+offset*4 : srcYStride+cardW*4]
			copy(dstRow[0:wFrom*4], srcFrom)
		}
		if wTo > 0 {
			srcTo := srcSlides[toIdx].Pix[srcYStride : srcYStride+offset*4]
			copy(dstRow[wFrom*4:cardW*4], srcTo)
		}
	}

	// Anti-aliased corner masking with rx=24 against slate background
	scaledRx := int(24.0*sx + 0.5)
	if scaledRx < 4 {
		scaledRx = 4
	}
	maskCardCorners(dst, adCard, scaledRx, colorBgSlate)

	// Bottom Carousel Indicators matching mobile frontend (.banner-indicators)
	if numSlides > 1 {
		totalWidth := float64((numSlides-1)*10 + 44 + (numSlides-1)*14)
		startX := 640.0 - totalWidth/2.0
		curX := int(startX*sx + 0.5)
		indY := int(955.0*sy + 0.5)
		indH := int(10.0*sy + 0.5)
		if indH < 4 {
			indH = 4
		}
		dotR := indH / 2
		if dotR < 2 {
			dotR = 2
		}

		for k := 0; k < numSlides; k++ {
			var w int
			var alpha uint8
			if isTransition {
				if k == fromIdx {
					w = int((44.0-34.0*easeProg)*sx + 0.5)
					alpha = uint8(255.0 - 140.0*easeProg + 0.5)
				} else if k == toIdx {
					w = int((10.0+34.0*easeProg)*sx + 0.5)
					alpha = uint8(115.0 + 140.0*easeProg + 0.5)
				} else {
					w = int(10.0*sx + 0.5)
					alpha = 115
				}
			} else {
				if k == toIdx {
					w = int(44.0*sx + 0.5)
					alpha = 255
				} else {
					w = int(10.0*sx + 0.5)
					alpha = 115
				}
			}
			if w < indH {
				w = indH
			}
			pill := image.Rect(curX, indY, curX+w, indY+indH)
			fillRoundedRect(dst, pill, dotR, color.RGBA{R: 255, G: 255, B: 255, A: alpha})
			curX += w + int(14.0*sx+0.5)
		}
	}

	return nil
}

func renderPINEntry(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Title
	drawText(dst, faceBold56, "Enter 4-Digit Pickup PIN", 90, 220, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Enter the 4-digit numeric PIN provided on your mobile order confirmation screen"
	}
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	// 4 PIN Boxes: 140px wide, 160px high, spaced 36px
	// Total width = 4*140 + 3*36 = 560 + 108 = 668px
	// Centered in card (dx=1860, minX=30): startX = 30 + (1860-668)/2 = 626
	boxW := 140
	boxH := 160
	boxGap := 36
	startX := 626
	boxY := 390

	pinStr := state.PIN
	for i := 0; i < 4; i++ {
		bX := startX + i*(boxW+boxGap)
		boxRect := image.Rect(bX, boxY, bX+boxW, boxY+boxH)

		hasDigit := i < len(pinStr)
		if hasDigit {
			fillRoundedRect(dst, boxRect, 18, colorPillBlueBg)
			strokeRoundedRect(dst, boxRect, 18, 3, colorPrimaryBlue)
			digitChar := string(pinStr[i])
			dW := measureTextWidth(faceBold56, digitChar)
			drawText(dst, faceBold56, digitChar, bX+(boxW-dW)/2, boxY+48, colorTextPrimary)
		} else {
			fillRoundedRect(dst, boxRect, 18, colorBgSlate)
			strokeRoundedRect(dst, boxRect, 18, 2, colorBorder)
			dash := "-"
			dW := measureTextWidth(faceBold56, dash)
			drawText(dst, faceBold56, dash, bX+(boxW-dW)/2, boxY+48, colorTextSecondary)
		}
	}

	// Error banner if any
	if strings.TrimSpace(state.PINError) != "" {
		errBox := image.Rect(450, 595, 1470, 665)
		drawPill(dst, errBox, colorPillRedBg, colorPillRedBorder)
		eW := measureTextWidth(faceBold22, state.PINError)
		drawText(dst, faceBold22, state.PINError, errBox.Min.X+(errBox.Dx()-eW)/2, 617, colorErrorRed)
	}

	// Guidance / Instructions box
	instBox := image.Rect(90, 715, 1830, 985)
	fillRoundedRect(dst, instBox, 20, colorBgSlate)
	strokeRoundedRect(dst, instBox, 20, 2, colorBorder)

	drawText(dst, faceBold28, "How the 4-digit pickup PIN works:", 140, 765, colorTextPrimary)
	drawText(dst, faceReg24, "1. Upload your file at xeroxgo-frontend.vercel.app from your phone camera or browser.", 140, 825, colorTextSecondary)
	drawText(dst, faceReg24, "2. A 4-digit pickup PIN is generated exclusively for your order (valid for 15 minutes).", 140, 875, colorTextSecondary)
	drawText(dst, faceReg24, "3. Enter your PIN on this kiosk to initiate secure UPI payment and collect your prints.", 140, 925, colorTextSecondary)
}

func renderPaymentQR(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Title
	drawText(dst, faceBold56, "Scan UPI QR to Pay & Print", 90, 220, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Use Google Pay, PhonePe, Paytm, BHIM, or any UPI banking app"
	}
	drawText(dst, faceReg32, msg, 90, 290, colorTextPrimary)

	// Left: QR Code Card (540x590)
	qrCard := image.Rect(90, 340, 670, 980)
	fillRoundedRect(dst, qrCard, 24, colorCardBg)
	strokeRoundedRect(dst, qrCard, 24, 2, colorBorder)

	qrBounds := image.Rect(145, 380, 615, 850)
	var qrImg image.Image
	if state.PaymentQRImage != nil {
		qrImg = state.PaymentQRImage
	} else if strings.TrimSpace(state.PaymentQRURL) != "" {
		if q, err := getQRCodeImage(state.PaymentQRURL); err == nil {
			qrImg = q
		}
	}

	if qrImg != nil {
		xdraw.BiLinear.Scale(dst, qrBounds, qrImg, qrImg.Bounds(), xdraw.Over, nil)
	} else {
		// Placeholder box when QR is loading
		fillRoundedRect(dst, qrBounds, 16, colorBgSlate)
		strokeRoundedRect(dst, qrBounds, 16, 2, colorBorder)
		loadingText := "Generating Secure UPI QR..."
		lW := measureTextWidth(faceBold24, loadingText)
		drawText(dst, faceBold24, loadingText, qrBounds.Min.X+(qrBounds.Dx()-lW)/2, 600, colorTextSecondary)
	}

	// QR footer note
	acceptedText := "Instant UPI Payment • Test Mode"
	aW := measureTextWidth(faceBold20, acceptedText)
	drawText(dst, faceBold20, acceptedText, qrCard.Min.X+(qrCard.Dx()-aW)/2, 900, colorPrimaryBlue)

	// Right: Payment & Order Details (width: 1100, height: 640)
	detailCard := image.Rect(720, 340, 1830, 980)
	fillRoundedRect(dst, detailCard, 24, colorBgSlate)
	strokeRoundedRect(dst, detailCard, 24, 2, colorBorder)

	// Amount Callout Banner
	amountBanner := image.Rect(760, 380, 1790, 520)
	fillRoundedRect(dst, amountBanner, 18, colorCardBg)
	strokeRoundedRect(dst, amountBanner, 18, 2, colorBorder)

	rupees := float64(state.AmountPaise) / 100.0
	amountStr := fmt.Sprintf("₹%.2f", rupees)
	drawText(dst, faceBold24, "Total Amount to Pay:", 800, 420, colorTextSecondary)
	drawText(dst, faceBold56, amountStr, 800, 465, colorPrimaryBlue)

	// Order metadata row
	if strings.TrimSpace(state.OrderID) != "" {
		orderPill := image.Rect(760, 550, 1180, 610)
		drawPill(dst, orderPill, colorPillBlueBg, colorPillBlueBorder)
		drawText(dst, faceBold22, "Order: "+state.OrderID, 785, 570, colorPrimaryBlue)
	}

	// Expiry countdown pill
	if !state.PaymentExpiresAt.IsZero() {
		rem := time.Until(state.PaymentExpiresAt)
		if rem > 0 {
			mins := int(rem.Minutes())
			secs := int(rem.Seconds()) % 60
			expText := fmt.Sprintf("QR expires in %02d:%02d", mins, secs)
			expPill := image.Rect(1220, 550, 1680, 610)
			drawPill(dst, expPill, colorPillBlueBg, colorPillBlueBorder)
			drawText(dst, faceBold22, expText, 1250, 570, colorPrimaryBlue)
		}
	}

	// Instructions
	instY := 655
	drawText(dst, faceBold28, "Payment Instructions:", 760, instY, colorTextPrimary)
	steps := []string{
		"1. Open any UPI application on your mobile device (GPay, PhonePe, Paytm, etc.).",
		"2. Point your camera at the QR code on the left and scan.",
		"3. Verify amount " + amountStr + " and authorize payment with your UPI PIN.",
		"4. Printing starts automatically as soon as payment is confirmed.",
	}
	for i, step := range steps {
		yPos := instY + 50 + i*48
		col := colorTextPrimary
		if i == 3 {
			col = colorSuccessGreen
		}
		drawText(dst, faceBold22, step, 760, yPos, col)
	}

	// Security note at bottom right
	drawText(dst, faceReg18, "🔒 End-to-end verified via Razorpay webhook. No cash or cards needed.", 760, 930, colorTextSecondary)
}

func renderWaitingForPayment(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Title
	drawText(dst, faceBold56, "Waiting for Payment...", 90, 220, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Please complete the UPI transfer on your mobile phone."
	}
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	// Amount pill
	rupees := float64(state.AmountPaise) / 100.0
	amountStr := fmt.Sprintf("₹%.2f", rupees)
	amountPill := image.Rect(90, 360, 480, 420)
	drawPill(dst, amountPill, colorPillBlueBg, colorPillBlueBorder)
	drawText(dst, faceBold28, "Amount: "+amountStr, 120, 377, colorPrimaryBlue)

	if strings.TrimSpace(state.OrderID) != "" {
		orderPill := image.Rect(510, 360, 920, 420)
		drawPill(dst, orderPill, colorBgSlate, colorBorder)
		drawText(dst, faceBold22, "Order ID: "+state.OrderID, 535, 380, colorTextSecondary)
	}

	// Main status container
	boxRect := image.Rect(90, 460, 1830, 860)
	fillRoundedRect(dst, boxRect, 20, colorBgSlate)
	strokeRoundedRect(dst, boxRect, 20, 2, colorBorder)

	statusText := state.PaymentStatus
	if strings.TrimSpace(statusText) == "" {
		statusText = "Awaiting bank confirmation..."
	}
	drawText(dst, faceBold36, statusText, 140, 530, colorTextPrimary)

	drawText(dst, faceBold24, "Do not leave this screen or navigate away.", 140, 610, colorPrimaryBlue)
	drawText(dst, faceReg24, "Your transaction is being confirmed by your bank and the Razorpay gateway.", 140, 670, colorTextSecondary)
	drawText(dst, faceReg24, "As soon as payment is confirmed, this screen will switch directly to printing.", 140, 730, colorTextSecondary)
	drawText(dst, faceReg24, "Thank you for your patience!", 140, 790, colorTextSecondary)
}

func renderPaymentFailed(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Title
	drawText(dst, faceBold56, "Payment Failed or Expired", 90, 220, colorErrorRed)

	msg := state.PaymentError
	if strings.TrimSpace(msg) == "" {
		msg = "The payment could not be completed or the time limit expired."
	}
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	// Status pill
	pill := image.Rect(90, 360, 650, 420)
	drawPill(dst, pill, colorPillRedBg, colorPillRedBorder)
	drawText(dst, faceBold22, "Status: Payment Incomplete", 120, 380, colorErrorRed)

	// Recovery instructions box
	boxRect := image.Rect(90, 460, 1830, 920)
	fillRoundedRect(dst, boxRect, 20, color.RGBA{R: 255, G: 251, B: 235, A: 255})
	strokeRoundedRect(dst, boxRect, 20, 2, color.RGBA{R: 253, G: 230, B: 138, A: 255})

	drawText(dst, faceBold28, "What to do next:", 140, 520, color.RGBA{R: 180, G: 83, B: 9, A: 255})

	if state.CanRetryPayment {
		drawText(dst, faceBold24, "1. Your payment session is still active! You can retry without re-entering your PIN.", 140, 580, colorTextPrimary)
		drawText(dst, faceBold24, "2. A new UPI QR code will be generated for your existing order.", 140, 640, colorTextPrimary)
		drawText(dst, faceReg24, "3. Check your UPI app to ensure your bank balance is sufficient.", 140, 700, colorTextSecondary)
		drawText(dst, faceReg24, "4. If you wish to cancel, this screen will automatically return to Idle.", 140, 760, colorTextSecondary)
	} else {
		drawText(dst, faceBold24, "1. Your pickup PIN or payment session has expired.", 140, 580, colorTextPrimary)
		drawText(dst, faceBold24, "2. Open your phone browser and refresh your order confirmation page.", 140, 640, colorTextPrimary)
		drawText(dst, faceReg24, "3. Generate a fresh 4-digit PIN and re-enter it at this kiosk.", 140, 700, colorTextSecondary)
		drawText(dst, faceReg24, "4. Your uploaded document is preserved and will not need to be uploaded again.", 140, 760, colorTextSecondary)
	}
}

func renderDownloading(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Bigger Title (56px) - Layman's terms
	drawText(dst, faceBold56, "Receiving Your Document...", 90, 220, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Your file is being sent to the printer. This only takes a few seconds."
	}
	// Bigger Subtitle (32px)
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	if strings.TrimSpace(state.Detail) != "" {
		detailPill := image.Rect(90, 360, 780, 420)
		drawPill(dst, detailPill, colorPillBlueBg, colorPillBlueBorder)
		drawText(dst, faceBold22, state.Detail, 120, 380, colorPrimaryBlue)
	}

	// Progress Bar (90, 500, 1830, 556) - width 1740, height 56
	drawProgressBar(dst, 90, 500, 1740, 56, state.ProgressPercent, 100, colorPrimaryBlue)

	// Bigger Progress Status (32px Bold)
	pctText := fmt.Sprintf("Getting ready: %d%% completed", state.ProgressPercent)
	drawText(dst, faceBold32, pctText, 90, 590, colorTextPrimary)

	// Bigger Guidance Note (24px) - Layman's terms
	drawText(dst, faceReg24, "Printing will start automatically as soon as the file finishes loading.", 90, 650, colorTextSecondary)
}

func renderPrinting(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Bigger Title (56px) - Layman's terms
	drawText(dst, faceBold56, "Printing Your Document...", 90, 220, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Your pages are printing right now. Please wait a moment..."
	}
	// Bigger Subtitle (32px)
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	// Counter Pill aligned in metadata row with larger font (28px Bold)
	counterText := fmt.Sprintf("Page %d of %d", state.CurrentPage, state.TotalPages)
	counterPill := image.Rect(90, 360, 350, 420)
	drawPill(dst, counterPill, colorPillBlueBg, colorPillBlueBorder)
	cW := measureTextWidth(faceBold28, counterText)
	drawText(dst, faceBold28, counterText, counterPill.Min.X+(counterPill.Dx()-cW)/2, 377, colorPrimaryBlue)

	if strings.TrimSpace(state.Detail) != "" {
		detailPill := image.Rect(375, 360, 1050, 420)
		drawPill(dst, detailPill, colorBgSlate, colorBorder)
		drawText(dst, faceBold22, state.Detail, 405, 380, colorTextSecondary)
	}

	// Progress Bar (90, 500, 1830, 556) - width 1740, height 56
	drawProgressBar(dst, 90, 500, 1740, 56, state.CurrentPage, state.TotalPages, colorPrimaryBlue)

	// Bigger Progress Status (32px Bold) - Layman's terms
	subProgress := fmt.Sprintf("Printing: %d of %d pages completed", state.CurrentPage, state.TotalPages)
	drawText(dst, faceBold32, subProgress, 90, 590, colorTextPrimary)

	// Bubble "Job is active on dcpt310..." removed as requested!
}

func renderCompleted(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Bigger Title (56px) - Layman's terms
	drawText(dst, faceBold56, "All Done! Printing Finished", 90, 220, colorSuccessGreen)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Your document has finished printing successfully."
	}
	// Bigger Subtitle (32px)
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	if strings.TrimSpace(state.Detail) != "" {
		detailPill := image.Rect(90, 360, 750, 420)
		drawPill(dst, detailPill, colorPillGreenBg, colorPillGreenBorder)
		drawText(dst, faceBold22, state.Detail, 120, 380, color.RGBA{R: 4, G: 120, B: 87, A: 255})
	}

	// Bigger Collection Hero Callout Banner (95px height, 32px font) - Layman's terms
	calloutBox := image.Rect(90, 450, 1830, 545)
	drawPill(dst, calloutBox, colorPillGreenBg, colorPillGreenBorder)
	calloutText := "Please take all your printed pages from the tray below"
	cW := measureTextWidth(faceBold32, calloutText)
	drawText(dst, faceBold32, calloutText, calloutBox.Min.X+(calloutBox.Dx()-cW)/2, 480, color.RGBA{R: 4, G: 120, B: 87, A: 255})

	// Instructions box with larger fonts (28px Bold / 24px) - Layman's terms
	instBox := image.Rect(90, 580, 1830, 830)
	fillRoundedRect(dst, instBox, 20, colorBgSlate)
	strokeRoundedRect(dst, instBox, 20, 2, colorBorder)

	drawText(dst, faceBold28, "1. Double check that you've collected all your pages.", 140, 635, colorTextPrimary)
	drawText(dst, faceBold28, "2. This screen will reset automatically for the next user.", 140, 700, colorTextPrimary)
	drawText(dst, faceReg24, "Thank you for using XeroxGo!", 140, 765, colorTextSecondary)
}

func renderError(dst *image.RGBA, state ScreenState) {
	card := image.Rect(30, 140, 1890, 1050)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	// Bigger Title (56px) - Layman's terms
	drawText(dst, faceBold56, "Printer Needs Attention", 90, 220, colorErrorRed)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "The printer has run out of paper or needs a quick check."
	}
	// Bigger Subtitle (32px)
	drawText(dst, faceReg32, msg, 90, 295, colorTextPrimary)

	if strings.TrimSpace(state.Detail) != "" {
		detailPill := image.Rect(90, 360, 750, 420)
		drawPill(dst, detailPill, colorPillRedBg, colorPillRedBorder)
		drawText(dst, faceBold22, state.Detail, 120, 380, colorErrorRed)
	}

	// Actionable recovery box - Layman's terms
	boxRect := image.Rect(90, 450, 1830, 970)
	fillRoundedRect(dst, boxRect, 20, color.RGBA{R: 255, G: 251, B: 235, A: 255})
	strokeRoundedRect(dst, boxRect, 20, 2, color.RGBA{R: 253, G: 230, B: 138, A: 255})

	drawText(dst, faceBold28, "What you can do to fix this:", 140, 500, color.RGBA{R: 180, G: 83, B: 9, A: 255})

	recoverySteps := []string{
		"1. Pull open the bottom paper tray and load standard A4 paper.",
		"2. If paper is stuck, gently remove it and firmly close the printer cover.",
		"3. Check that the printed output tray is empty and clear.",
		"4. If it still doesn't print, please alert the shop or campus attendant.",
	}
	yR := 575
	for _, step := range recoverySteps {
		drawText(dst, faceBold24, step, 140, yR, colorTextPrimary)
		yR += 70
	}
}

func progressBarFillWidth(innerBarWidth, current, total int) int {
	if innerBarWidth <= 0 || total <= 0 || current <= 0 {
		return 0
	}
	if current >= total {
		return innerBarWidth
	}
	return (innerBarWidth * current) / total
}

func drawProgressBar(dst *image.RGBA, x, y, width, height, current, total int, fillColor color.RGBA) {
	outerRect := image.Rect(x, y, x+width, y+height)
	fillRect(dst, outerRect, colorBarBg)
	strokeRect(dst, outerRect, 2, colorBorder)

	innerW := width - 4
	innerH := height - 4
	fillW := progressBarFillWidth(innerW, current, total)
	if fillW > 0 {
		fillRect(dst, image.Rect(x+2, y+2, x+2+fillW, y+2+innerH), fillColor)
	}
}

func fillRect(dst *image.RGBA, r image.Rectangle, col color.RGBA) {
	if dst == nil {
		return
	}
	r = r.Intersect(dst.Bounds())
	if r.Empty() || col.A == 0 {
		return
	}
	rowBytes := r.Dx() * 4
	if col.A == 255 {
		firstOff := dst.PixOffset(r.Min.X, r.Min.Y)
		firstRow := dst.Pix[firstOff : firstOff+rowBytes]
		firstRow[0] = col.R
		firstRow[1] = col.G
		firstRow[2] = col.B
		firstRow[3] = 255
		for bp := 4; bp < rowBytes; bp *= 2 {
			copy(firstRow[bp:], firstRow[:bp])
		}
		for y := r.Min.Y + 1; y < r.Max.Y; y++ {
			off := dst.PixOffset(r.Min.X, y)
			copy(dst.Pix[off:off+rowBytes], firstRow)
		}
		return
	}

	alpha := float64(col.A) / 255.0
	invAlpha := 1.0 - alpha
	rF := float64(col.R) * alpha
	gF := float64(col.G) * alpha
	bF := float64(col.B) * alpha

	for y := r.Min.Y; y < r.Max.Y; y++ {
		off := dst.PixOffset(r.Min.X, y)
		row := dst.Pix[off : off+rowBytes]
		for p := 0; p < rowBytes; p += 4 {
			row[p+0] = uint8(rF + float64(row[p+0])*invAlpha)
			row[p+1] = uint8(gF + float64(row[p+1])*invAlpha)
			row[p+2] = uint8(bF + float64(row[p+2])*invAlpha)
			row[p+3] = 255
		}
	}
}

func strokeRect(dst *image.RGBA, r image.Rectangle, thickness int, col color.RGBA) {
	if thickness <= 0 {
		return
	}
	fillRect(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+thickness), col)
	fillRect(dst, image.Rect(r.Min.X, r.Max.Y-thickness, r.Max.X, r.Max.Y), col)
	fillRect(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+thickness, r.Max.Y), col)
	fillRect(dst, image.Rect(r.Max.X-thickness, r.Min.Y, r.Max.X, r.Max.Y), col)
}

func drawTextScaled(dst *image.RGBA, text string, startX, startY, scale int, col color.RGBA) {
	initUIAssets()
	var face font.Face
	switch scale {
	case 1:
		face = faceReg16
	case 2:
		face = faceReg18
	case 3:
		face = faceReg24
	case 4:
		face = faceBold36
	default:
		face = faceBold48
	}
	drawText(dst, face, text, startX, startY, col)
}

// FilePreviewSink atomically writes a 1920x1080 RGBA image to pngPath via a temporary file.
func FilePreviewSink(pngPath string) Sink {
	return func(img *image.RGBA) error {
		if strings.TrimSpace(pngPath) == "" {
			return fmt.Errorf("pngPath cannot be empty")
		}
		if img == nil {
			return fmt.Errorf("image cannot be nil")
		}

		dir := filepath.Dir(pngPath)
		dirStat, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("destination directory does not exist: %w", err)
		}
		if !dirStat.IsDir() {
			return fmt.Errorf("destination parent path is not a directory")
		}

		tmpFile, err := os.CreateTemp(dir, "preview_*.png.tmp")
		if err != nil {
			return fmt.Errorf("failed to create temporary preview file: %w", err)
		}
		tmpName := tmpFile.Name()

		cleanup := true
		defer func() {
			if cleanup {
				_ = tmpFile.Close()
				_ = os.Remove(tmpName)
			}
		}()

		if err := encodeFastPNG(tmpFile, img); err != nil {
			return fmt.Errorf("failed to encode PNG: %w", err)
		}

		if err := tmpFile.Sync(); err != nil {
			return fmt.Errorf("failed to sync temp file: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			return fmt.Errorf("failed to close temp file: %w", err)
		}

		if err := os.Rename(tmpName, pngPath); err != nil {
			_ = os.Remove(pngPath)
			if err2 := os.Rename(tmpName, pngPath); err2 != nil {
				return fmt.Errorf("failed to atomically rename preview file: %w", err)
			}
		}

		cleanup = false
		return nil
	}
}

// FramebufferGeometry holds the detected physical display dimensions and pixel layout.
type FramebufferGeometry struct {
	Width        int
	Height       int
	BitsPerPixel int
	Stride       int
}

// FBGeometry is an alias for FramebufferGeometry for test compatibility.
type FBGeometry = FramebufferGeometry

// DetectFramebufferGeometry reads geometry parameters from sysfs.
func DetectFramebufferGeometry(fbPath string) FramebufferGeometry {
	geom := FramebufferGeometry{
		Width:        1920,
		Height:       1080,
		BitsPerPixel: 32,
		Stride:       1920 * 4,
	}

	fbBase := filepath.Base(fbPath)
	sysfsDir := filepath.Join("/sys/class/graphics", fbBase)

	if sizeBytes, err := os.ReadFile(filepath.Join(sysfsDir, "virtual_size")); err == nil {
		parts := strings.Split(strings.TrimSpace(string(sizeBytes)), ",")
		if len(parts) == 2 {
			if w, werr := strconv.Atoi(parts[0]); werr == nil && w > 0 {
				geom.Width = w
			}
			if h, herr := strconv.Atoi(parts[1]); herr == nil && h > 0 {
				geom.Height = h
			}
		}
	}

	if bppBytes, err := os.ReadFile(filepath.Join(sysfsDir, "bits_per_pixel")); err == nil {
		if bpp, perr := strconv.Atoi(strings.TrimSpace(string(bppBytes))); perr == nil && bpp > 0 {
			geom.BitsPerPixel = bpp
		}
	}

	strideRead := false
	for _, name := range []string{"stride", "line_length"} {
		if strideBytes, err := os.ReadFile(filepath.Join(sysfsDir, name)); err == nil {
			if s, serr := strconv.Atoi(strings.TrimSpace(string(strideBytes))); serr == nil && s > 0 {
				geom.Stride = s
				strideRead = true
				break
			}
		}
	}
	if !strideRead {
		bytesPerPixel := geom.BitsPerPixel / 8
		if bytesPerPixel < 1 {
			bytesPerPixel = 4
		}
		geom.Stride = geom.Width * bytesPerPixel
	}

	return geom
}

// FramebufferSink returns a Sink that maps canvas to /dev/fb0 using auto-detected geometry.
func FramebufferSink(fbPath string) Sink {
	return FramebufferSinkWithGeometry(fbPath, DetectFramebufferGeometry(fbPath))
}

// FramebufferSinkWithGeometry writes canvas pixels to /dev/fb0, scaling and encoding as required.
func FramebufferSinkWithGeometry(fbPath string, geom FramebufferGeometry) Sink {
	var (
		convMu     sync.Mutex
		convBuf    []byte
		cachedFile *os.File
		cachedMmap []byte
		scaledImg  *image.RGBA
	)

	return func(img *image.RGBA) error {
		if strings.TrimSpace(fbPath) == "" {
			return fmt.Errorf("framebuffer path cannot be empty")
		}
		if img == nil {
			return fmt.Errorf("image cannot be nil")
		}

		convMu.Lock()
		defer convMu.Unlock()

		if cachedFile == nil {
			f, err := os.OpenFile(fbPath, os.O_RDWR, 0)
			if err != nil {
				return fmt.Errorf("failed to open framebuffer %s: %w", fbPath, err)
			}
			cachedFile = f

			totalBytes := geom.Stride * geom.Height
			info, sErr := cachedFile.Stat()
			if sErr == nil && (info.Mode()&os.ModeDevice != 0) && totalBytes > 0 {
				if m, mErr := mmapFB(cachedFile.Fd(), totalBytes); mErr == nil {
					cachedMmap = m
				}
			}
		}

		canvasBounds := img.Bounds()
		canvasW := canvasBounds.Dx()
		canvasH := canvasBounds.Dy()

		var targetImg *image.RGBA
		if geom.Width == canvasW && geom.Height == canvasH {
			targetImg = img
		} else {
			if scaledImg == nil || scaledImg.Bounds().Dx() != geom.Width || scaledImg.Bounds().Dy() != geom.Height {
				scaledImg = image.NewRGBA(image.Rect(0, 0, geom.Width, geom.Height))
			}
			xdraw.BiLinear.Scale(scaledImg, scaledImg.Bounds(), img, canvasBounds, xdraw.Over, nil)
			targetImg = scaledImg
		}

		targetW := geom.Width
		targetH := geom.Height

		totalBytes := geom.Stride * targetH
		if len(convBuf) < totalBytes {
			convBuf = make([]byte, totalBytes)
		}
		buf := convBuf[:totalBytes]

		switch geom.BitsPerPixel {
		case 16:
			// 4x4 Bayer matrix ordered dithering for smooth 16-bit RGB565 conversion
			bayer4 := [4][4]int{
				{0, 8, 2, 10},
				{12, 4, 14, 6},
				{3, 11, 1, 9},
				{15, 7, 13, 5},
			}
			for y := 0; y < targetH; y++ {
				rowOffset := y * geom.Stride
				srcRowOffset := y * targetImg.Stride
				bRow := bayer4[y&3]
				for x := 0; x < targetW; x++ {
					p := srcRowOffset + x*4
					d := bRow[x&3]

					// Red (5 bits, step = 8, dither range [-3, +4])
					rVal := int(targetImg.Pix[p+0]) + (d >> 1) - 3
					if rVal < 0 {
						rVal = 0
					} else if rVal > 255 {
						rVal = 255
					}

					// Green (6 bits, step = 4, dither range [-1, +2])
					gVal := int(targetImg.Pix[p+1]) + (d >> 2) - 1
					if gVal < 0 {
						gVal = 0
					} else if gVal > 255 {
						gVal = 255
					}

					// Blue (5 bits, step = 8, dither range [-3, +4])
					bVal := int(targetImg.Pix[p+2]) + (d >> 1) - 3
					if bVal < 0 {
						bVal = 0
					} else if bVal > 255 {
						bVal = 255
					}

					rgb565 := (uint16(rVal>>3) << 11) | (uint16(gVal>>2) << 5) | uint16(bVal>>3)
					destIdx := rowOffset + x*2
					buf[destIdx+0] = byte(rgb565 & 0xFF)
					buf[destIdx+1] = byte((rgb565 >> 8) & 0xFF)
				}
			}

		default:
			// Ultra-fast streaming 1D conversion if strides match width * 4
			if geom.Stride == targetW*4 && targetImg.Stride == targetW*4 {
				srcPix := targetImg.Pix
				for p := 0; p < totalBytes; p += 4 {
					buf[p+0] = srcPix[p+2] // B
					buf[p+1] = srcPix[p+1] // G
					buf[p+2] = srcPix[p+0] // R
					buf[p+3] = srcPix[p+3] // A
				}
			} else {
				for y := 0; y < targetH; y++ {
					rowOffset := y * geom.Stride
					srcRowOffset := y * targetImg.Stride
					for x := 0; x < targetW; x++ {
						p := srcRowOffset + x*4
						destIdx := rowOffset + x*4
						buf[destIdx+0] = targetImg.Pix[p+2] // B
						buf[destIdx+1] = targetImg.Pix[p+1] // G
						buf[destIdx+2] = targetImg.Pix[p+0] // R
						buf[destIdx+3] = targetImg.Pix[p+3] // A
					}
				}
			}
		}

		if cachedMmap != nil && len(cachedMmap) >= totalBytes {
			// Synchronize with hardware VBLANK on Linux framebuffer to eliminate shearing/tearing
			syncVBlank(cachedFile.Fd())
			copy(cachedMmap[:totalBytes], buf)
			return nil
		}

		if _, err := cachedFile.WriteAt(buf, 0); err != nil {
			_ = cachedFile.Close()
			cachedFile = nil
			cachedMmap = nil
			return fmt.Errorf("write framebuffer: %w", err)
		}
		if fi, err := cachedFile.Stat(); err == nil && (fi.Mode()&os.ModeDevice == 0) {
			_ = cachedFile.Close()
			cachedFile = nil
		}
		return nil
	}
}
