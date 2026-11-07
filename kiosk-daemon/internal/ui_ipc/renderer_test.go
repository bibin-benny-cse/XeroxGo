package ui_ipc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

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
	defer f.Close()

	decoded, err := png.Decode(f)
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

