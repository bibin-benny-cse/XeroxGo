package ui_ipc

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	xdraw "golang.org/x/image/draw"
	"xeroxgo/pkg/contracts"
)

// Test A — TestRenderAllModes
func TestRenderAllModes(t *testing.T) {
	testCases := []struct {
		name  string
		state ScreenState
	}{
		{
			name: "idle mode",
			state: ScreenState{
				Mode:      ModeIdle,
				QRContent: "https://kiosk.xeroxgo.edu/session/test-session-123",
				Message:   "Ready for new print job",
			},
		},
		{
			name: "pin entry mode with leading zero",
			state: ScreenState{
				Mode: ModePINEntry,
				PIN:  "0482",
			},
		},
		{
			name: "pin entry mode with error",
			state: ScreenState{
				Mode:     ModePINEntry,
				PIN:      "04",
				PINError: "Invalid or expired pickup PIN. Please check your phone.",
			},
		},
		{
			name: "payment qr mode",
			state: ScreenState{
				Mode:             ModePaymentQR,
				OrderID:          "ord_test123",
				AmountPaise:      1500,
				PaymentQRURL:     "upi://pay?pa=xeroxgo@icici&pn=XeroxGo&am=15.00&tr=ord_test123",
				PaymentExpiresAt: time.Now().Add(10 * time.Minute),
			},
		},
		{
			name: "waiting for payment mode",
			state: ScreenState{
				Mode:          ModeWaitingForPayment,
				OrderID:       "ord_test123",
				AmountPaise:   1500,
				PaymentStatus: "Contacting payment gateway...",
			},
		},
		{
			name: "payment failed mode with retry",
			state: ScreenState{
				Mode:            ModePaymentFailed,
				PaymentError:    "Payment attempt timed out",
				CanRetryPayment: true,
			},
		},
		{
			name: "payment failed mode without retry",
			state: ScreenState{
				Mode:            ModePaymentFailed,
				PaymentError:    "Session expired",
				CanRetryPayment: false,
			},
		},
		{
			name: "downloading mode",
			state: ScreenState{
				Mode:            ModeDownloading,
				ProgressPercent: 65,
				Message:         "Fetching document from cloud...",
				Detail:          "Job ID: job-456",
			},
		},
		{
			name: "printing mode",
			state: ScreenState{
				Mode:        ModePrinting,
				CurrentPage: 3,
				TotalPages:  7,
				Message:     "Streaming pages to Brother HL-L5210DN",
				Detail:      "Job ID: job-456",
			},
		},
		{
			name: "completed mode",
			state: ScreenState{
				Mode:    ModeCompleted,
				Message: "Printing complete",
				Detail:  "All 7 pages printed",
			},
		},
		{
			name: "error mode",
			state: ScreenState{
				Mode:    ModeError,
				Message: "Printer tray 1 empty",
				Detail:  "Refill standard A4 paper to resume",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := Render(tc.state)
			if err != nil {
				t.Fatalf("Render failed for %s: %v", tc.name, err)
			}
			if img == nil {
				t.Fatalf("Render returned nil image for %s", tc.name)
			}
			expectedBounds := image.Rect(0, 0, ScreenWidth, ScreenHeight)
			if img.Bounds() != expectedBounds {
				t.Fatalf("unexpected bounds for %s: got %v, want %v", tc.name, img.Bounds(), expectedBounds)
			}
		})
	}
}

// Test B — TestRenderIdleQRCode
func TestRenderIdleQRCode(t *testing.T) {
	qrContent := "https://kiosk.xeroxgo.edu/auth/kiosk-99?token=xyz"
	state := ScreenState{
		Mode:      ModeIdle,
		QRContent: qrContent,
	}

	img, err := Render(state)
	if err != nil {
		t.Fatalf("Render(ModeIdle) failed: %v", err)
	}
	if img == nil {
		t.Fatalf("Render(ModeIdle) returned nil image")
	}
	if img.Bounds() != image.Rect(0, 0, ScreenWidth, ScreenHeight) {
		t.Fatalf("invalid canvas bounds: %v", img.Bounds())
	}

	// QR code is drawn at (1380, 210, 1780, 610)
	qrBounds := image.Rect(1380, 210, 1780, 610)
	darkCount := 0
	lightCount := 0

	for y := qrBounds.Min.Y; y < qrBounds.Max.Y; y++ {
		for x := qrBounds.Min.X; x < qrBounds.Max.X; x++ {
			c := img.RGBAAt(x, y)
			// Dark pixels (QR modules)
			if c.R < 50 && c.G < 50 && c.B < 50 && c.A == 255 {
				darkCount++
			}
			// Light pixels (QR quiet zone / white modules)
			if c.R > 200 && c.G > 200 && c.B > 200 && c.A == 255 {
				lightCount++
			}
		}
	}

	if darkCount == 0 {
		t.Fatalf("expected dark QR modules in QR region, found none")
	}
	if lightCount == 0 {
		t.Fatalf("expected light QR background/modules in QR region, found none")
	}
	if darkCount+lightCount == 0 {
		t.Fatalf("QR area was completely empty or transparent")
	}
}

// Test C — TestRenderProgressBars
func TestRenderProgressBars(t *testing.T) {
	innerW := 1556 // 1560 - 4

	// 1. Download progress monotonic increase test
	w0 := progressBarFillWidth(innerW, 0, 100)
	w50 := progressBarFillWidth(innerW, 50, 100)
	w100 := progressBarFillWidth(innerW, 100, 100)

	if w0 != 0 {
		t.Fatalf("expected 0 fill at 0%%, got %d", w0)
	}
	if w50 <= w0 {
		t.Fatalf("expected 50%% fill (%d) > 0%% fill (%d)", w50, w0)
	}
	if w100 <= w50 {
		t.Fatalf("expected 100%% fill (%d) > 50%% fill (%d)", w100, w50)
	}
	if w100 != innerW {
		t.Fatalf("expected 100%% fill to equal full inner bar width (%d), got %d", innerW, w100)
	}

	// 2. Printing page-progress bar test
	p0 := progressBarFillWidth(innerW, 0, 10)
	p5 := progressBarFillWidth(innerW, 5, 10)
	p10 := progressBarFillWidth(innerW, 10, 10)

	if p0 != 0 {
		t.Fatalf("expected 0 fill at page 0/10, got %d", p0)
	}
	if p5 <= p0 {
		t.Fatalf("expected page 5 fill (%d) > page 0 fill (%d)", p5, p0)
	}
	if p10 <= p5 {
		t.Fatalf("expected page 10 fill (%d) > page 5 fill (%d)", p10, p5)
	}
	if p10 != innerW {
		t.Fatalf("expected page 10/10 fill to equal full inner bar width (%d), got %d", innerW, p10)
	}

	// 3. Pixel verification on rendered download screens
	img0, err := Render(ScreenState{Mode: ModeDownloading, ProgressPercent: 0})
	if err != nil {
		t.Fatalf("Render 0%% failed: %v", err)
	}
	img50, err := Render(ScreenState{Mode: ModeDownloading, ProgressPercent: 50})
	if err != nil {
		t.Fatalf("Render 50%% failed: %v", err)
	}
	img100, err := Render(ScreenState{Mode: ModeDownloading, ProgressPercent: 100})
	if err != nil {
		t.Fatalf("Render 100%% failed: %v", err)
	}

	// Bar X is 140, inner bar starts at 142. Bar Y is 500, middle Y is 525.
	// At 0%: start of inner bar should be empty (colorBarBg)
	c0 := img0.RGBAAt(145, 525)
	if c0 != colorBarBg {
		t.Fatalf("expected colorBarBg at (145, 525) for 0%% progress, got %v", c0)
	}

	// At 50%: start of inner bar should be accent blue, end should be colorBarBg
	c50Start := img50.RGBAAt(145, 525)
	if c50Start != colorAccentBlue {
		t.Fatalf("expected colorAccentBlue at (145, 525) for 50%% progress, got %v", c50Start)
	}
	c50End := img50.RGBAAt(1680, 525)
	if c50End != colorBarBg {
		t.Fatalf("expected colorBarBg at (1680, 525) for 50%% progress, got %v", c50End)
	}

	// At 100%: end of inner bar should be filled with accent blue
	c100End := img100.RGBAAt(1680, 525)
	if c100End != colorAccentBlue {
		t.Fatalf("expected colorAccentBlue at (1680, 525) for 100%% progress, got %v", c100End)
	}
}

// Test D — TestRenderRejectsInvalidState
func TestRenderRejectsInvalidState(t *testing.T) {
	testCases := []struct {
		name  string
		state ScreenState
	}{
		{
			name:  "unknown mode",
			state: ScreenState{Mode: "UNKNOWN_MODE"},
		},
		{
			name:  "idle with empty QR content",
			state: ScreenState{Mode: ModeIdle, QRContent: ""},
		},
		{
			name:  "idle with whitespace-only QR content",
			state: ScreenState{Mode: ModeIdle, QRContent: "   \t\n  "},
		},
		{
			name:  "download progress below 0",
			state: ScreenState{Mode: ModeDownloading, ProgressPercent: -1},
		},
		{
			name:  "download progress above 100",
			state: ScreenState{Mode: ModeDownloading, ProgressPercent: 101},
		},
		{
			name:  "printing with zero total pages",
			state: ScreenState{Mode: ModePrinting, CurrentPage: 0, TotalPages: 0},
		},
		{
			name:  "printing with negative current page",
			state: ScreenState{Mode: ModePrinting, CurrentPage: -1, TotalPages: 5},
		},
		{
			name:  "printing with current page greater than total pages",
			state: ScreenState{Mode: ModePrinting, CurrentPage: 6, TotalPages: 5},
		},
		{
			name:  "pin entry with PIN exceeding 4 digits",
			state: ScreenState{Mode: ModePINEntry, PIN: "12345"},
		},
		{
			name:  "payment qr with negative amount",
			state: ScreenState{Mode: ModePaymentQR, AmountPaise: -1},
		},
		{
			name:  "waiting for payment with negative amount",
			state: ScreenState{Mode: ModeWaitingForPayment, AmountPaise: -10},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := Render(tc.state)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if img != nil {
				t.Fatalf("expected nil image on error for %s", tc.name)
			}
		})
	}
}

// Test E — TestRenderDeterministic
func TestRenderDeterministic(t *testing.T) {
	state := ScreenState{
		Mode:      ModeIdle,
		QRContent: "https://kiosk.xeroxgo.edu/session/deterministic-test",
		Message:   "Deterministic test message",
		Detail:    "Detail info",
	}

	img1, err1 := Render(state)
	if err1 != nil {
		t.Fatalf("Render 1 failed: %v", err1)
	}

	img2, err2 := Render(state)
	if err2 != nil {
		t.Fatalf("Render 2 failed: %v", err2)
	}

	if !bytes.Equal(img1.Pix, img2.Pix) {
		t.Fatalf("rendered images for identical state are not pixel-identical")
	}
}

// Test F — TestFilePreviewSink
func TestFilePreviewSink(t *testing.T) {
	tmpDir := t.TempDir()
	previewPath := filepath.Join(tmpDir, "preview.png")

	sink := FilePreviewSink(previewPath)

	state := ScreenState{
		Mode:      ModeIdle,
		QRContent: "https://kiosk.xeroxgo.edu/session/preview-test",
	}
	img, err := Render(state)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	if err := sink(img); err != nil {
		t.Fatalf("FilePreviewSink write failed: %v", err)
	}

	// Verify output exists and is non-empty
	info, err := os.Stat(previewPath)
	if err != nil {
		t.Fatalf("preview file stat failed: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("preview file is empty")
	}

	// Verify decoded PNG
	f, err := os.Open(previewPath)
	if err != nil {
		t.Fatalf("failed to open preview file: %v", err)
	}

	decoded, err := png.Decode(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("png.Decode failed on written preview: %v", err)
	}
	if decoded.Bounds() != image.Rect(0, 0, ScreenWidth, ScreenHeight) {
		t.Fatalf("unexpected decoded bounds: %v", decoded.Bounds())
	}

	// Second write to same path
	state2 := ScreenState{
		Mode:            ModeDownloading,
		ProgressPercent: 50,
	}
	img2, err := Render(state2)
	if err != nil {
		t.Fatalf("Render 2 failed: %v", err)
	}

	if err := sink(img2); err != nil {
		t.Fatalf("FilePreviewSink second write failed: %v", err)
	}

	// Verify no temporary files remain in directory
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read temp dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file in directory, found %d: %v", len(entries), entries)
	}
	if entries[0].Name() != "preview.png" {
		t.Fatalf("unexpected file in directory: %s", entries[0].Name())
	}
}

// Test G — TestFilePreviewSinkErrors
func TestFilePreviewSinkErrors(t *testing.T) {
	tmpDir := t.TempDir()
	validImg := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))

	// 1. Empty destination path
	sinkEmpty := FilePreviewSink("")
	if err := sinkEmpty(validImg); err == nil {
		t.Fatalf("expected error for empty pngPath, got nil")
	}

	// 2. Nil image input
	sinkValid := FilePreviewSink(filepath.Join(tmpDir, "test.png"))
	if err := sinkValid(nil); err == nil {
		t.Fatalf("expected error for nil image, got nil")
	}

	// 3. Destination in nonexistent directory
	sinkNonexistentDir := FilePreviewSink(filepath.Join(tmpDir, "nonexistent_dir", "test.png"))
	if err := sinkNonexistentDir(validImg); err == nil {
		t.Fatalf("expected error for nonexistent destination directory, got nil")
	}
}

// Test H — TestFramebufferSinkBGRA
func TestFramebufferSinkBGRA(t *testing.T) {
	tmpDir := t.TempDir()
	fbPath := filepath.Join(tmpDir, "fb0.raw")

	// Pre-create the output file to simulate existing fb0
	if err := os.WriteFile(fbPath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to pre-create fb file: %v", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))

	// Top-left pixel
	img.SetRGBA(0, 0, color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0x44})

	// Secondary known pixel at (100, 200)
	img.SetRGBA(100, 200, color.RGBA{R: 0xAA, G: 0xBB, B: 0xCC, A: 0xDD})

	sink := FramebufferSink(fbPath)
	if err := sink(img); err != nil {
		t.Fatalf("FramebufferSink write failed: %v", err)
	}

	data, err := os.ReadFile(fbPath)
	if err != nil {
		t.Fatalf("failed to read fb output: %v", err)
	}

	expectedLen := ScreenWidth * ScreenHeight * 4
	if len(data) != expectedLen {
		t.Fatalf("unexpected file size: got %d, want %d", len(data), expectedLen)
	}

	// Top-left pixel: expected B=0x33, G=0x22, R=0x11, A=0x44
	if data[0] != 0x33 || data[1] != 0x22 || data[2] != 0x11 || data[3] != 0x44 {
		t.Fatalf("top-left pixel mismatch: got [%x %x %x %x], want [33 22 11 44]",
			data[0], data[1], data[2], data[3])
	}

	// Coordinate (100, 200): offset = (200 * ScreenWidth + 100) * 4
	offset := (200*ScreenWidth + 100) * 4
	if data[offset+0] != 0xCC || data[offset+1] != 0xBB || data[offset+2] != 0xAA || data[offset+3] != 0xDD {
		t.Fatalf("pixel at (100, 200) mismatch: got [%x %x %x %x], want [cc bb aa dd]",
			data[offset+0], data[offset+1], data[offset+2], data[offset+3])
	}
}

// Test I — TestFramebufferSinkErrors
func TestFramebufferSinkErrors(t *testing.T) {
	tmpDir := t.TempDir()
	validImg := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))

	// 1. Empty path
	sinkEmpty := FramebufferSink("")
	if err := sinkEmpty(validImg); err == nil {
		t.Fatalf("expected error for empty fbPath, got nil")
	}

	// 2. Nil image
	sinkValid := FramebufferSink(filepath.Join(tmpDir, "dummy_fb"))
	if err := sinkValid(nil); err == nil {
		t.Fatalf("expected error for nil image, got nil")
	}

	// 3. Incorrect dimensions (not 1920x1080)
	smallImg := image.NewRGBA(image.Rect(0, 0, 800, 600))
	if err := sinkValid(smallImg); err == nil {
		t.Fatalf("expected error for 800x600 image dimensions, got nil")
	}

	// 4. Nonexistent framebuffer path (must not create file)
	nonexistentPath := filepath.Join(tmpDir, "nonexistent_fb0")
	sinkNonexistent := FramebufferSink(nonexistentPath)
	if err := sinkNonexistent(validImg); err == nil {
		t.Fatalf("expected error for nonexistent framebuffer path, got nil")
	}
	if _, statErr := os.Stat(nonexistentPath); !os.IsNotExist(statErr) {
		t.Fatalf("nonexistent framebuffer path should not have been created")
	}
}

// Test J — TestFramebufferSinkWritesCompleteImage
func TestFramebufferSinkWritesCompleteImage(t *testing.T) {
	tmpDir := t.TempDir()
	fbPath := filepath.Join(tmpDir, "fb_complete.raw")

	if err := os.WriteFile(fbPath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create fb file: %v", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	// Fill with a deterministic pattern
	for y := 0; y < ScreenHeight; y++ {
		for x := 0; x < ScreenWidth; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}

	sink := FramebufferSink(fbPath)
	if err := sink(img); err != nil {
		t.Fatalf("FramebufferSink failed: %v", err)
	}

	info, err := os.Stat(fbPath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	expectedLen := int64(ScreenWidth * ScreenHeight * 4)
	if info.Size() != expectedLen {
		t.Fatalf("file size mismatch: got %d, want %d", info.Size(), expectedLen)
	}

	// Sample check multiple rows to ensure no truncation or row skipping
	f, err := os.Open(fbPath)
	if err != nil {
		t.Fatalf("failed to open output: %v", err)
	}
	defer f.Close()

	sampleRows := []int{0, 1, 539, 1079}
	rowBuf := make([]byte, ScreenWidth*4)

	for _, row := range sampleRows {
		offset := int64(row * ScreenWidth * 4)
		n, err := f.ReadAt(rowBuf, offset)
		if err != nil {
			t.Fatalf("failed to read row %d: %v", row, err)
		}
		if n != len(rowBuf) {
			t.Fatalf("short read on row %d: got %d bytes", row, n)
		}

		// Check first and last pixel of row
		expectedB0 := uint8((0 + row) % 256)
		expectedG0 := uint8(row % 256)
		expectedR0 := uint8(0 % 256)
		expectedA0 := uint8(255)

		if rowBuf[0] != expectedB0 || rowBuf[1] != expectedG0 || rowBuf[2] != expectedR0 || rowBuf[3] != expectedA0 {
			t.Fatalf("row %d pixel 0 mismatch: got [%v %v %v %v], want [%v %v %v %v]",
				row, rowBuf[0], rowBuf[1], rowBuf[2], rowBuf[3], expectedB0, expectedG0, expectedR0, expectedA0)
		}

		lastX := ScreenWidth - 1
		lastIdx := lastX * 4
		expectedBLast := uint8((lastX + row) % 256)
		expectedGLast := uint8(row % 256)
		expectedRLast := uint8(lastX % 256)
		expectedALast := uint8(255)

		if rowBuf[lastIdx+0] != expectedBLast || rowBuf[lastIdx+1] != expectedGLast || rowBuf[lastIdx+2] != expectedRLast || rowBuf[lastIdx+3] != expectedALast {
			t.Fatalf("row %d pixel %d mismatch: got [%v %v %v %v], want [%v %v %v %v]",
				row, lastX, rowBuf[lastIdx+0], rowBuf[lastIdx+1], rowBuf[lastIdx+2], rowBuf[lastIdx+3],
				expectedBLast, expectedGLast, expectedRLast, expectedALast)
		}
	}
}

func TestFramebufferSinkRGB565Scaled(t *testing.T) {
	tmpDir := t.TempDir()
	fbPath := filepath.Join(tmpDir, "fb_720p_16bpp.raw")

	if err := os.WriteFile(fbPath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create fb file: %v", err)
	}

	geom := FBGeometry{
		Width:        1280,
		Height:       720,
		BitsPerPixel: 16,
		Stride:       2560,
	}

	// 1920x1080 canvas filled with solid red (255, 0, 0)
	img := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	for y := 0; y < ScreenHeight; y++ {
		for x := 0; x < ScreenWidth; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}

	sink := FramebufferSinkWithGeometry(fbPath, geom)
	if err := sink(img); err != nil {
		t.Fatalf("FramebufferSinkWithGeometry failed: %v", err)
	}

	data, err := os.ReadFile(fbPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	expectedLen := 720 * 2560
	if len(data) != expectedLen {
		t.Fatalf("file size mismatch: got %d, want %d", len(data), expectedLen)
	}

	// Red in RGB565 is 0xF800, which in little-endian bytes is [0x00, 0xF8]
	if data[0] != 0x00 || data[1] != 0xF8 {
		t.Fatalf("pixel 0 mismatch: got [%x %x], want [00 f8]", data[0], data[1])
	}
}

func TestRenderAdSlidesGradients(t *testing.T) {
	expectedCenterColors := [3]color.RGBA{
		{R: 37, G: 99, B: 235, A: 255},  // Slide 0: Center 50% Primary Blue #2563EB
		{R: 16, G: 185, B: 129, A: 255}, // Slide 1: Center 50% Emerald Green #10B981
		{R: 124, G: 58, B: 237, A: 255}, // Slide 2: Center 50% Violet #7C3AED
	}

	for i, expected := range expectedCenterColors {
		img, err := Render(ScreenState{
			Mode:         ModeIdle,
			QRContent:    "https://kiosk.test/idle",
			AdSlideIndex: i,
		})
		if err != nil {
			t.Fatalf("Render slide %d failed: %v", i, err)
		}

		// Sample along center diagonal (t = 0.5) where dx + dy = (1220 + 910) / 2 = 1065
		// e.g. dx = 700 (x = 730), dy = 365 (y = 505)
		sampledCenter := img.RGBAAt(730, 505)
		if sampledCenter != expected {
			t.Fatalf("slide %d center gradient color mismatch at (730, 505): got %v, want %v", i, sampledCenter, expected)
		}

		// Verify rounded corner masking at outer corner (30, 140) against Slate background
		cornerSample := img.RGBAAt(30, 140)
		if cornerSample != colorBgSlate {
			t.Fatalf("slide %d outer corner (30, 140) not masked with colorBgSlate: got %v, want %v", i, cornerSample, colorBgSlate)
		}
	}
}

func TestRenderAdSlideTransition(t *testing.T) {
	// Transitioning from Slide 0 (Blue gradient) to Slide 1 (Green gradient) at 50%
	img, err := Render(ScreenState{
		Mode:           ModeIdle,
		QRContent:      "https://kiosk.test/idle",
		AdSlideIndex:   1,
		TransitionFrom: 0,
		TransitionProg: 0.5,
	})
	if err != nil {
		t.Fatalf("Render transition failed: %v", err)
	}

	// At 50% transition, card width is 1203.
	// Outgoing Slide 0 occupies the left half, incoming Slide 1 occupies the right half.
	leftSample := img.RGBAAt(300, 200)
	if leftSample.B < 200 || leftSample.R > 60 {
		t.Fatalf("expected left side of transition card to be Slide 0 Blue gradient, got %v", leftSample)
	}

	rightSample := img.RGBAAt(1000, 200)
	if rightSample.G < 120 || rightSample.B > 100 {
		t.Fatalf("expected right side of transition card to be Slide 1 Green gradient, got %v", rightSample)
	}

	// Verify corner masking still intact during active transition
	cornerSample := img.RGBAAt(30, 140)
	if cornerSample != colorBgSlate {
		t.Fatalf("transition outer corner (30, 140) not masked with colorBgSlate: got %v, want %v", cornerSample, colorBgSlate)
	}
}

func TestConfigureTarget720p(t *testing.T) {
	// Configure 1280x720 target
	ConfigureTarget(FramebufferGeometry{
		Width:        1280,
		Height:       720,
		BitsPerPixel: 32,
		Stride:       1280 * 4,
	})
	defer ConfigureTarget(FramebufferGeometry{
		Width:        ScreenWidth,
		Height:       ScreenHeight,
		BitsPerPixel: 32,
		Stride:       ScreenWidth * 4,
	})

	tw, th := GetTargetResolution()
	if tw != 1280 || th != 720 {
		t.Fatalf("unexpected target resolution: got %dx%d, want 1280x720", tw, th)
	}

	img, err := Render(ScreenState{
		Mode:           ModeIdle,
		QRContent:      "https://kiosk.test/720p",
		AdSlideIndex:   1,
		TransitionFrom: 0,
		TransitionProg: 0.5,
	})
	if err != nil {
		t.Fatalf("Render 720p failed: %v", err)
	}

	expectedBounds := image.Rect(0, 0, 1280, 720)
	if img.Bounds() != expectedBounds {
		t.Fatalf("unexpected image bounds for 720p: got %v, want %v", img.Bounds(), expectedBounds)
	}

	// Verify that the left card contains Slide 0 Blue gradient and right side contains Slide 1 Green gradient
	// Scaled card: x ~ 25 to 827, y ~ 93 to 653
	leftSample := img.RGBAAt(200, 150)
	if leftSample.B < 180 || leftSample.R > 70 {
		t.Fatalf("expected left side of 720p transition card to be Slide 0 Blue, got %v", leftSample)
	}
	rightSample := img.RGBAAt(650, 150)
	if rightSample.G < 110 || rightSample.B > 110 {
		t.Fatalf("expected right side of 720p transition card to be Slide 1 Green, got %v", rightSample)
	}
}

func TestConfigureAdsDynamic(t *testing.T) {
	defer ConfigureAds(nil)

	customAds := []contracts.AdItem{
		{
			ID:          "custom-1",
			Title:       "Special Promo",
			Description: "50% off campus printing this week only.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#7C2D12",
			GradientC1:  "#EA580C",
			GradientC2:  "#F59E0B",
			SortOrder:   1,
			IsActive:    true,
		},
		{
			ID:          "custom-2",
			Title:       "Night Owl Hours",
			Description: "Kiosks open 24/7 in library common area.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#0F172A",
			GradientC1:  "#334155",
			GradientC2:  "#1E293B",
			SortOrder:   2,
			IsActive:    true,
		},
	}

	ConfigureAds(customAds)

	count := GetAdSlideCount()
	if count != 2 {
		t.Fatalf("expected slide count 2, got %d", count)
	}

	img0 := GetAdSlideImage(0)
	if img0 == nil {
		t.Fatal("expected non-nil image for slide 0")
	}

	img, err := Render(ScreenState{
		Mode:           ModeIdle,
		QRContent:      "https://kiosk.test/custom",
		AdSlideIndex:   0,
		TransitionFrom: -1,
	})
	if err != nil {
		t.Fatalf("Render failed with custom ads: %v", err)
	}
	if img == nil {
		t.Fatal("expected non-nil render output")
	}
}

func TestConfigureAdsConcurrentSameThreeAds(t *testing.T) {
	defer ConfigureAds(nil)

	threeAds := []contracts.AdItem{
		{
			ID:          "ad-quick-easy",
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
			ID:          "ad-high-quality",
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
			ID:          "ad-skip-queue",
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

	const workers = 16
	const iterations = 25

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				ConfigureAds(threeAds)
			}
		}()
	}
	close(start)
	wg.Wait()

	if count := GetAdSlideCount(); count != 3 {
		t.Fatalf("expected 3 ad slides after concurrent ConfigureAds, got %d", count)
	}
	for idx := 0; idx < 3; idx++ {
		slide := GetAdSlideImage(idx)
		if slide == nil {
			t.Fatalf("expected non-nil slide image at index %d", idx)
		}
		if slide.Bounds().Dx() != 1220 || slide.Bounds().Dy() != 910 {
			t.Fatalf("unexpected slide %d bounds: %v", idx, slide.Bounds())
		}
	}
}

func TestConfigureAdsConcurrentWithRenderTo(t *testing.T) {
	defer ConfigureAds(nil)
	initUIAssets()

	adsA := []contracts.AdItem{
		{
			ID:          "ad-a1",
			Title:       "WWQQMM@@88 Quick & Easy",
			Description: "iiii....llll!!!! Direct UPI payment ready in seconds.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#1E3A8A",
			GradientC1:  "#2563EB",
			GradientC2:  "#4F46E5",
			SortOrder:   1,
			IsActive:    true,
		},
		{
			ID:          "ad-a2",
			Title:       "High Quality Prints @ 1200 DPI",
			Description: "Crisp laser documents ready in seconds.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#065F46",
			GradientC1:  "#10B981",
			GradientC2:  "#059669",
			SortOrder:   2,
			IsActive:    true,
		},
		{
			ID:          "ad-a3",
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

	adsB := []contracts.AdItem{
		{
			ID:          "ad-b1",
			Title:       "i.i.l.l! Tiny Narrow Glyphs",
			Description: "WWWWQQQQMMMM@@@@8888 Wide glyphs in description.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#7C2D12",
			GradientC1:  "#EA580C",
			GradientC2:  "#F59E0B",
			SortOrder:   1,
			IsActive:    true,
		},
		{
			ID:          "ad-b2",
			Title:       "Campus Instant Print",
			Description: "Secure pickup via single-use 4-digit PIN.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#0F172A",
			GradientC1:  "#334155",
			GradientC2:  "#1E293B",
			SortOrder:   2,
			IsActive:    true,
		},
		{
			ID:          "ad-b3",
			Title:       "Eco Duplex Saving",
			Description: "Save paper with automatic double-sided printing.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#1E3A8A",
			GradientC1:  "#2563EB",
			GradientC2:  "#4F46E5",
			SortOrder:   3,
			IsActive:    true,
		},
	}

	const adWorkers = 6
	const renderWorkers = 6
	const iterations = 20

	start := make(chan struct{})
	errCh := make(chan error, renderWorkers*iterations)
	var wg sync.WaitGroup

	for w := 0; w < adWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				if (workerID+i)%2 == 0 {
					ConfigureAds(adsA)
				} else {
					ConfigureAds(adsB)
				}
			}
		}(w)
	}

	for w := 0; w < renderWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			dst := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
			<-start
			for i := 0; i < iterations; i++ {
				var st ScreenState
				switch i % 6 {
				case 0:
					st = ScreenState{
						Mode:           ModeIdle,
						QRContent:      fmt.Sprintf("https://kiosk.test/concurrent/%d/%d", workerID, i),
						AdSlideIndex:   i % 3,
						TransitionFrom: -1,
					}
				case 1:
					st = ScreenState{
						Mode:    ModePINEntry,
						PIN:     fmt.Sprintf("%04d", (workerID*100+i)%10000),
						Message: "Enter 4-digit pickup PIN",
					}
				case 2:
					st = ScreenState{
						Mode:             ModePaymentQR,
						OrderID:          fmt.Sprintf("ord_%d_%d", workerID, i),
						AmountPaise:      600,
						PaymentQRURL:     "https://rzp.io/i/XG-CONCURRENT",
						PaymentExpiresAt: time.Now().Add(10 * time.Minute),
					}
				case 3:
					st = ScreenState{
						Mode:            ModeDownloading,
						ProgressPercent: (i * 5) % 101,
						Detail:          "Downloading PDF document...",
					}
				case 4:
					st = ScreenState{
						Mode:        ModePrinting,
						CurrentPage: (i % 5) + 1,
						TotalPages:  5,
						Detail:      "Single-sided • 1 Copy",
					}
				case 5:
					st = ScreenState{
						Mode:   ModeCompleted,
						Detail: "5 Sheets Printed",
					}
				}
				if err := RenderTo(dst, st); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("RenderTo failed during concurrent execution: %v", err)
	}
	if count := GetAdSlideCount(); count != 3 {
		t.Fatalf("expected 3 ad slides after concurrent RenderTo + ConfigureAds, got %d", count)
	}
}

func TestFontHelpersConcurrentDrawAndMeasure(t *testing.T) {
	initUIAssets()

	const workers = 12
	const iterations = 40
	start := make(chan struct{})
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			dst := image.NewRGBA(image.Rect(0, 0, 800, 200))
			<-start
			for i := 0; i < iterations; i++ {
				if (workerID+i)%2 == 0 {
					_ = measureTextWidth(faceBold56, "WWQQMM@@88 Quick & Easy")
					drawText(dst, faceBold56, "WWQQMM@@88 Quick & Easy", 20, 20, colorPrimaryBlue)
					_ = measureTextWidth(faceReg28, "Pick up your print job with your 4-digit code.")
					drawText(dst, faceReg28, "Pick up your print job with your 4-digit code.", 20, 100, colorTextPrimary)
				} else {
					_ = measureTextWidth(faceBold56, "iiii....llll!!!! 0482")
					drawText(dst, faceBold56, "iiii....llll!!!! 0482", 20, 20, colorTextPrimary)
					_ = measureTextWidth(faceReg28, "i.l!.")
					drawText(dst, faceReg28, "i.l!.", 20, 100, colorTextSecondary)
				}
			}
		}(w)
	}

	close(start)
	wg.Wait()
}

func TestInitAdSlidesDoesNotOverwriteExplicitConfigureAds(t *testing.T) {
	initUIAssets()
	defer func() {
		ConfigureTarget(FramebufferGeometry{
			Width:        ScreenWidth,
			Height:       ScreenHeight,
			BitsPerPixel: 32,
			Stride:       ScreenWidth * 4,
		})
		ConfigureAds(nil)
	}()

	serverAds := []contracts.AdItem{
		{
			ID:          "srv-orange-1",
			Title:       "Server Promo One",
			Description: "Custom server banner must not be replaced by defaults.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#7C2D12",
			GradientC1:  "#EA580C",
			GradientC2:  "#F59E0B",
			SortOrder:   1,
			IsActive:    true,
		},
		{
			ID:          "srv-orange-2",
			Title:       "Server Promo Two",
			Description: "Active promotional configuration from cloud.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#9A3412",
			GradientC1:  "#F97316",
			GradientC2:  "#FBBF24",
			SortOrder:   2,
			IsActive:    true,
		},
		{
			ID:          "srv-orange-3",
			Title:       "Server Promo Three",
			Description: "Verified server-provided ad slide.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#7C2D12",
			GradientC1:  "#EA580C",
			GradientC2:  "#F59E0B",
			SortOrder:   3,
			IsActive:    true,
		},
	}

	const rounds = 15
	for round := 0; round < rounds; round++ {
		ConfigureTarget(FramebufferGeometry{
			Width:        ScreenWidth,
			Height:       ScreenHeight,
			BitsPerPixel: 32,
			Stride:       ScreenWidth * 4,
		})

		// Reset to an empty ad slide state before launching concurrent callers.
		adSlidesMu.Lock()
		adSlideImgs = nil
		scaledAdSlideImgs = nil
		lastAdConfigSeq = 0
		adSlidesMu.Unlock()

		start := make(chan struct{})
		errCh := make(chan error, 8)
		var wg sync.WaitGroup

		// Default initialization workers racing against explicit server ads
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				initAdSlides()
			}()
		}

		// Explicit server ad configuration
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ConfigureAds(serverAds)
		}()

		// Overlapping RenderTo calls across Idle and non-Idle modes
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				dst := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
				<-start
				for i := 0; i < 4; i++ {
					if err := RenderTo(dst, ScreenState{
						Mode:           ModeIdle,
						QRContent:      fmt.Sprintf("https://kiosk.test/init-race/%d/%d/%d", round, workerID, i),
						AdSlideIndex:   i % 3,
						TransitionFrom: -1,
					}); err != nil {
						errCh <- err
						return
					}
					if err := RenderTo(dst, ScreenState{
						Mode:    ModePINEntry,
						PIN:     "0482",
						Message: "Enter 4-digit pickup PIN",
					}); err != nil {
						errCh <- err
						return
					}
				}
			}(w)
		}

		// Concurrent target geometry configuration (verify no deadlock with ConfigureTarget)
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			<-start
			if r%2 == 0 {
				ConfigureTarget(FramebufferGeometry{Width: 1280, Height: 720, BitsPerPixel: 32, Stride: 1280 * 4})
			} else {
				ConfigureTarget(FramebufferGeometry{Width: ScreenWidth, Height: ScreenHeight, BitsPerPixel: 32, Stride: ScreenWidth * 4})
			}
		}(round)

		close(start)
		wg.Wait()
		close(errCh)

		for err := range errCh {
			t.Fatalf("round %d: RenderTo failed: %v", round, err)
		}

		if count := GetAdSlideCount(); count != 3 {
			t.Fatalf("round %d: expected 3 active server ad slides, got %d", round, count)
		}
		slide0 := GetAdSlideImage(0)
		if slide0 == nil {
			t.Fatalf("round %d: GetAdSlideImage(0) returned nil", round)
		}
		// Server slide 0 uses warm orange (#EA580C), whereas default slide 0 uses blue (#2563EB).
		centerPixel := slide0.RGBAAt(600, 450)
		if centerPixel.R < 180 || centerPixel.B > 60 {
			t.Fatalf("round %d: expected server ad slide (orange, R>180 B<60) to remain active, got default/other pixel %v", round, centerPixel)
		}
	}
}

func seedPatternCanvas(bounds image.Rectangle, extraStride int) *image.RGBA {
	w := bounds.Dx()
	h := bounds.Dy()
	stride := w*4 + extraStride
	pix := make([]byte, h*stride)
	img := &image.RGBA{
		Pix:    pix,
		Stride: stride,
		Rect:   bounds,
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x*7 + y*3) & 0xFF),
				G: uint8((x*5 + y*11) & 0xFF),
				B: uint8((x*13 + y*17) & 0xFF),
				A: 255,
			})
		}
	}
	return img
}

func fillRectRef(dst *image.RGBA, r image.Rectangle, col color.RGBA) {
	if dst == nil {
		return
	}
	r = r.Intersect(dst.Bounds())
	if r.Empty() || col.A == 0 {
		return
	}
	if col.A == 255 {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				off := dst.PixOffset(x, y)
				dst.Pix[off+0] = col.R
				dst.Pix[off+1] = col.G
				dst.Pix[off+2] = col.B
				dst.Pix[off+3] = 255
			}
		}
		return
	}
	alpha := float64(col.A) / 255.0
	invAlpha := 1.0 - alpha
	rF := float64(col.R) * alpha
	gF := float64(col.G) * alpha
	bF := float64(col.B) * alpha
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			off := dst.PixOffset(x, y)
			dst.Pix[off+0] = uint8(rF + float64(dst.Pix[off+0])*invAlpha)
			dst.Pix[off+1] = uint8(gF + float64(dst.Pix[off+1])*invAlpha)
			dst.Pix[off+2] = uint8(bF + float64(dst.Pix[off+2])*invAlpha)
			dst.Pix[off+3] = 255
		}
	}
}

func assertImagesPixelEqual(t *testing.T, label string, got, want *image.RGBA) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("%s bounds mismatch: got %v, want %v", label, got.Bounds(), want.Bounds())
	}
	b := got.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			gp := got.RGBAAt(x, y)
			wp := want.RGBAAt(x, y)
			if gp != wp {
				t.Fatalf("%s pixel mismatch at (%d,%d): got %v, want %v", label, x, y, gp, wp)
			}
		}
	}
}

func TestFillRectPixelEquivalence(t *testing.T) {
	canvases := []struct {
		name        string
		bounds      image.Rectangle
		extraStride int
		subRect     *image.Rectangle
	}{
		{name: "zero_origin_tight_stride", bounds: image.Rect(0, 0, 120, 80), extraStride: 0},
		{name: "zero_origin_padded_stride", bounds: image.Rect(0, 0, 97, 65), extraStride: 36},
		{name: "nonzero_bounds_tight_stride", bounds: image.Rect(40, 25, 160, 115), extraStride: 0},
		{name: "nonzero_bounds_padded_stride", bounds: image.Rect(15, 20, 133, 98), extraStride: 48},
		{
			name:        "subimage_nonzero_and_stride",
			bounds:      image.Rect(0, 0, 200, 160),
			extraStride: 24,
			subRect:     func() *image.Rectangle { r := image.Rect(30, 25, 145, 110); return &r }(),
		},
	}

	colors := []color.RGBA{
		{R: 37, G: 99, B: 235, A: 255},   // opaque
		{R: 255, G: 255, B: 255, A: 255}, // opaque white
		{R: 16, G: 185, B: 129, A: 0},    // zero alpha
		{R: 239, G: 68, B: 68, A: 128},   // mid alpha
		{R: 15, G: 23, B: 42, A: 1},      // edge alpha 1
		{R: 245, G: 158, B: 11, A: 254},  // edge alpha 254
	}

	for _, cSpec := range canvases {
		b := cSpec.bounds
		rects := []struct {
			name string
			r    image.Rectangle
		}{
			{"single_pixel", image.Rect(b.Min.X+5, b.Min.Y+5, b.Min.X+6, b.Min.Y+6)},
			{"odd_width_inside", image.Rect(b.Min.X+3, b.Min.Y+4, b.Min.X+36, b.Min.Y+29)},
			{"clip_left_top", image.Rect(b.Min.X-15, b.Min.Y-10, b.Min.X+30, b.Min.Y+25)},
			{"clip_right_bottom", image.Rect(b.Max.X-25, b.Max.Y-20, b.Max.X+35, b.Max.Y+15)},
			{"clip_all_sides", image.Rect(b.Min.X-20, b.Min.Y-20, b.Max.X+20, b.Max.Y+20)},
			{"completely_outside", image.Rect(b.Max.X+10, b.Max.Y+10, b.Max.X+50, b.Max.Y+50)},
			{"empty_rect", image.Rect(b.Min.X+10, b.Min.Y+10, b.Min.X+10, b.Min.Y+20)},
		}

		for _, rSpec := range rects {
			for _, col := range colors {
				gotParent := seedPatternCanvas(cSpec.bounds, cSpec.extraStride)
				wantParent := seedPatternCanvas(cSpec.bounds, cSpec.extraStride)

				gotDst := gotParent
				wantDst := wantParent
				targetRect := rSpec.r
				if cSpec.subRect != nil {
					gotDst = gotParent.SubImage(*cSpec.subRect).(*image.RGBA)
					wantDst = wantParent.SubImage(*cSpec.subRect).(*image.RGBA)
					// Shift test rects around subRect coordinates too
					subR := rSpec.r.Intersect(*cSpec.subRect)
					if rSpec.name == "clip_left_top" {
						targetRect = image.Rect(cSpec.subRect.Min.X-8, cSpec.subRect.Min.Y-6, cSpec.subRect.Min.X+20, cSpec.subRect.Min.Y+15)
					} else if rSpec.name == "clip_right_bottom" {
						targetRect = image.Rect(cSpec.subRect.Max.X-15, cSpec.subRect.Max.Y-12, cSpec.subRect.Max.X+12, cSpec.subRect.Max.Y+10)
					} else if rSpec.name == "completely_outside" {
						targetRect = image.Rect(cSpec.subRect.Max.X+2, cSpec.subRect.Max.Y+2, cSpec.subRect.Max.X+12, cSpec.subRect.Max.Y+12)
					} else if !subR.Empty() {
						targetRect = subR
					}
				}

				fillRect(gotDst, targetRect, col)
				fillRectRef(wantDst, targetRect, col)

				label := cSpec.name + "/" + rSpec.name
				assertImagesPixelEqual(t, label+" (target)", gotDst, wantDst)
				// Also verify parent pixels outside subimage remain untouched
				if cSpec.subRect != nil {
					assertImagesPixelEqual(t, label+" (parent)", gotParent, wantParent)
				}
			}
		}
	}
}

func TestDrawLogoPixelEquivalenceAndConcurrency(t *testing.T) {
	initUIAssets()
	if rawLogoImg == nil {
		t.Fatal("rawLogoImg was not initialized")
	}

	drawLogoRef := func(dst *image.RGBA, x, y, targetHeight int) {
		b := rawLogoImg.Bounds()
		if b.Empty() || targetHeight <= 0 {
			return
		}
		targetWidth := (targetHeight * b.Dx()) / b.Dy()
		if targetWidth <= 0 {
			return
		}
		dstRect := image.Rect(x, y, x+targetWidth, y+targetHeight)
		xdraw.BiLinear.Scale(dst, dstRect, rawLogoImg, b, xdraw.Over, nil)
	}

	cases := []struct {
		name   string
		canvas func() (*image.RGBA, *image.RGBA)
		x, y   int
		h      int
	}{
		{
			name: "white_header_48",
			canvas: func() (*image.RGBA, *image.RGBA) {
				b := image.Rect(0, 0, 600, 120)
				c := image.NewRGBA(b)
				fillRect(c, b, colorCardBg)
				return c, nil
			},
			x: 80, y: 31, h: 48,
		},
		{
			name: "clipped_left_top_32",
			canvas: func() (*image.RGBA, *image.RGBA) {
				b := image.Rect(0, 0, 300, 80)
				return seedPatternCanvas(b, 16), nil
			},
			x: -25, y: -10, h: 32,
		},
		{
			name: "clipped_right_bottom_40",
			canvas: func() (*image.RGBA, *image.RGBA) {
				b := image.Rect(0, 0, 300, 80)
				return seedPatternCanvas(b, 20), nil
			},
			x: 240, y: 55, h: 40,
		},
		{
			name: "completely_outside_48",
			canvas: func() (*image.RGBA, *image.RGBA) {
				b := image.Rect(0, 0, 200, 100)
				return seedPatternCanvas(b, 0), nil
			},
			x: 300, y: 200, h: 48,
		},
		{
			name: "nonzero_subimage_stride_64",
			canvas: func() (*image.RGBA, *image.RGBA) {
				parent := seedPatternCanvas(image.Rect(0, 0, 420, 200), 32)
				sub := parent.SubImage(image.Rect(20, 15, 340, 160)).(*image.RGBA)
				return sub, parent
			},
			x: 40, y: 30, h: 64,
		},
		{
			name: "zero_and_negative_height",
			canvas: func() (*image.RGBA, *image.RGBA) {
				b := image.Rect(0, 0, 100, 100)
				return seedPatternCanvas(b, 0), nil
			},
			x: 10, y: 10, h: 0,
		},
	}

	for _, tc := range cases {
		gotDst, gotParent := tc.canvas()
		wantDst, wantParent := tc.canvas()

		drawLogo(gotDst, tc.x, tc.y, tc.h)
		drawLogoRef(wantDst, tc.x, tc.y, tc.h)

		assertImagesPixelEqual(t, tc.name, gotDst, wantDst)
		if gotParent != nil && wantParent != nil {
			assertImagesPixelEqual(t, tc.name+" (parent)", gotParent, wantParent)
		}
	}

	// Verify concurrent safety and immutability of cached entries across goroutines
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			h := 48
			if idx%2 == 1 {
				h = 36
			}
			dst := image.NewRGBA(image.Rect(0, 0, 500, 120))
			fillRect(dst, dst.Bounds(), colorCardBg)
			drawLogo(dst, 20, 20, h)
		}(i)
	}
	wg.Wait()
}

func BenchmarkFillRectOpaque1080p(b *testing.B) {
	dst := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	rect := dst.Bounds()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillRect(dst, rect, colorBgSlate)
	}
}

func BenchmarkDrawLogo48(b *testing.B) {
	initUIAssets()
	dst := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	fillRect(dst, dst.Bounds(), colorCardBg)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drawLogo(dst, 80, 31, 48)
	}
}

func BenchmarkRenderToPINEntry(b *testing.B) {
	initUIAssets()
	dst := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	st := ScreenState{
		Mode:    ModePINEntry,
		PIN:     "0482",
		Message: "Enter your 4-digit pickup PIN",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := RenderTo(dst, st); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderToPaymentQR(b *testing.B) {
	initUIAssets()
	dst := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	st := ScreenState{
		Mode:         ModePaymentQR,
		OrderID:      "ord_bench_001",
		AmountPaise:  300,
		PaymentQRURL: "https://rzp.io/i/XG-BENCH-001",
		Message:      "Scan with any UPI app to pay",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := RenderTo(dst, st); err != nil {
			b.Fatal(err)
		}
	}
}
