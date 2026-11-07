package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skip2/go-qrcode"
)

const (
	width  = 1920
	height = 1080
)

type adSlideDef struct {
	title  string
	desc   string
	c0     string
	c1     string
	c2     string
	active int
}

var defaultSlides = []adSlideDef{
	{
		title:  "Quick &amp; Easy",
		desc:   "Direct UPI payment. No coins, no cash needed.",
		c0:     "#1E3A8A",
		c1:     "#2563EB",
		c2:     "#4F46E5",
		active: 0,
	},
	{
		title:  "High Quality Prints",
		desc:   "Crisp laser documents ready in seconds.",
		c0:     "#065F46",
		c1:     "#10B981",
		c2:     "#059669",
		active: 1,
	},
	{
		title:  "Skip The Queue",
		desc:   "Pick up your print job with your 4-digit code.",
		c0:     "#581C87",
		c1:     "#7C3AED",
		c2:     "#D97706",
		active: 2,
	},
}

func main() {
	outDir := "design/figma"
	if err := os.MkdirAll(outDir, 0755); err != nil {
		panic(err)
	}

	logoBytes, err := os.ReadFile("frontend/assets/xeroxgo-full-logo-transparent.png")
	if err != nil {
		logoBytes, _ = os.ReadFile("frontend/assets/xeroxgo-logo.png")
	}
	logoB64 := base64.StdEncoding.EncodeToString(logoBytes)

	// Generate QR code PNG and base64 encode
	qrPng, _ := qrcode.Encode("https://xeroxgo.vercel.app", qrcode.Medium, 400)
	qrB64 := base64.StdEncoding.EncodeToString(qrPng)

	screens := []struct {
		filename string
		title    string
		svg      string
	}{
		{"01_idle.svg", "Idle Screen (Slide 1 - Quick & Easy)", buildCurrentIdleSVG(logoB64, qrB64, defaultSlides[0], 1)},
		{"01_idle_slide2.svg", "Idle Screen (Slide 2 - High Quality Prints)", buildCurrentIdleSVG(logoB64, qrB64, defaultSlides[1], 2)},
		{"01_idle_slide3.svg", "Idle Screen (Slide 3 - Skip The Queue)", buildCurrentIdleSVG(logoB64, qrB64, defaultSlides[2], 3)},
		{"02_downloading.svg", "Downloading Screen", buildDownloadingSVG(logoB64)},
		{"03_printing.svg", "Printing Screen", buildPrintingSVG(logoB64)},
		{"04_completed.svg", "Print Completed Screen", buildCompletedSVG(logoB64)},
		{"05_error.svg", "Attention & Hardware Alert Screen", buildErrorSVG(logoB64)},
	}

	for _, s := range screens {
		path := filepath.Join(outDir, s.filename)
		if err := os.WriteFile(path, []byte(s.svg), 0644); err != nil {
			panic(err)
		}
		fmt.Printf("Exported %s\n", path)
	}

	// Generate unified multi-frame artboard for Figma
	masterArtboard := buildMasterArtboardSVG(screens)
	artboardPath := filepath.Join(outDir, "all_screens_artboard.svg")
	if err := os.WriteFile(artboardPath, []byte(masterArtboard), 0644); err != nil {
		panic(err)
	}
	fmt.Printf("Exported %s\n", artboardPath)
}

func svgHeader(title string, extraDefs string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg width="1920" height="1080" viewBox="0 0 1920 1080" fill="none" xmlns="http://www.w3.org/2000/svg">
  <title>%s</title>
  <defs>
    <style>
      @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&amp;display=swap');
      text { font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
    </style>
%s  </defs>
  <!-- Background Slate-50 -->
  <rect width="1920" height="1080" fill="#F8FAFC"/>
`, title, extraDefs)
}

func buildCommonHeader(logoB64 string) string {
	return fmt.Sprintf(`
  <!-- HEADER -->
  <g id="Header">
    <rect width="1920" height="110" fill="#FFFFFF"/>
    <line x1="0" y1="108" x2="1920" y2="108" stroke="#E2E8F0" stroke-width="2"/>
    <image x="80" y="31" width="360" height="48" href="data:image/png;base64,%s" preserveAspectRatio="xMidYMid meet"/>
    <text x="480" y="64" fill="#64748B" font-size="22" font-weight="500">Upload, Print, Collect - It's that simple!</text>
  </g>
`, logoB64)
}

func buildCurrentIdleSVG(logoB64, qrB64 string, slide adSlideDef, slideNum int) string {
	var buf bytes.Buffer
	gradID := fmt.Sprintf("adGrad%d", slideNum)
	extraDefs := fmt.Sprintf(`    <linearGradient id="%s" x1="0%%" y1="0%%" x2="100%%" y2="100%%">
      <stop offset="0%%" stop-color="%s"/>
      <stop offset="50%%" stop-color="%s"/>
      <stop offset="100%%" stop-color="%s"/>
    </linearGradient>
`, gradID, slide.c0, slide.c1, slide.c2)

	buf.WriteString(svgHeader(fmt.Sprintf("XeroxGo Kiosk - Idle Screen (Slide %d)", slideNum), extraDefs))
	buf.WriteString(buildCommonHeader(logoB64))

	// Indicators: Slide 1, 2, 3
	dot1W, dot2W, dot3W := 10, 10, 10
	dot1A, dot2A, dot3A := "0.45", "0.45", "0.45"
	switch slide.active {
	case 0:
		dot1W = 44
		dot1A = "1.0"
	case 1:
		dot2W = 44
		dot2A = "1.0"
	case 2:
		dot3W = 44
		dot3A = "1.0"
	}

	buf.WriteString(fmt.Sprintf(`
  <!-- MAIN CONTENT: DUAL CARDS -->
  <g id="IdleCards">
    <!-- LEFT CARD: PROMOTIONAL AD BANNER CAROUSEL -->
    <g id="LeftCard_AdSlide">
      <rect x="30" y="140" width="1220" height="910" rx="24" fill="url(#%s)"/>
      
      <!-- Slide Content -->
      <g id="SlideText" transform="translate(110, 480)">
        <text x="0" y="0" fill="#FFFFFF" font-size="60" font-weight="800" letter-spacing="-0.02em">%s</text>
        <text x="0" y="75" fill="#E2E8F0" font-size="30" font-weight="400">%s</text>
      </g>

      <!-- Carousel Indicator Dots -->
      <g id="CarouselIndicators" transform="translate(595, 955)">
        <rect x="0" y="0" width="%d" height="10" rx="5" fill="#FFFFFF" opacity="%s"/>
        <rect x="%d" y="0" width="%d" height="10" rx="5" fill="#FFFFFF" opacity="%s"/>
        <rect x="%d" y="0" width="%d" height="10" rx="5" fill="#FFFFFF" opacity="%s"/>
      </g>
    </g>

    <!-- RIGHT CARD: QR CODE & MOBILE ONBOARDING -->
    <g id="RightCard_QROnboarding">
      <rect x="1280" y="140" width="610" height="910" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>
      
      <!-- Headline & Subtitle -->
      <text x="1585" y="186" fill="#1E293B" font-size="28" font-weight="700" text-anchor="middle">Scan QR to Get Started</text>
      <text x="1585" y="214" fill="#334155" font-size="20" font-weight="700" text-anchor="middle">Point your phone camera here, or visit website below</text>

      <!-- QR Code Image (360x360) -->
      <g id="QRCode" transform="translate(1405, 236)">
        <image width="360" height="360" href="data:image/png;base64,%s" preserveAspectRatio="xMidYMid meet"/>
      </g>

      <!-- Action Pill with Website Link -->
      <g id="ActionPill" transform="translate(1355, 626)">
        <rect width="460" height="56" rx="28" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="2"/>
        <text x="230" y="36" fill="#2563EB" font-size="24" font-weight="700" text-anchor="middle">xeroxgo.vercel.app</text>
      </g>

      <!-- 3 Numbered Steps -->
      <g id="Steps" transform="translate(1348, 716)">
        <!-- Step 1 -->
        <g id="Step1" transform="translate(0, 0)">
          <circle cx="18" cy="18" r="18" fill="#2563EB"/>
          <text x="18" y="25" fill="#FFFFFF" font-size="18" font-weight="700" text-anchor="middle">1</text>
          <text x="50" y="25" fill="#1E293B" font-size="22" font-weight="700">Upload your file &amp; choose print settings</text>
        </g>

        <!-- Step 2 -->
        <g id="Step2" transform="translate(0, 68)">
          <circle cx="18" cy="18" r="18" fill="#2563EB"/>
          <text x="18" y="25" fill="#FFFFFF" font-size="18" font-weight="700" text-anchor="middle">2</text>
          <text x="50" y="25" fill="#1E293B" font-size="22" font-weight="700">Pay with UPI (GPay, PhonePe, Paytm)</text>
        </g>

        <!-- Step 3 -->
        <g id="Step3" transform="translate(0, 136)">
          <circle cx="18" cy="18" r="18" fill="#10B981"/>
          <text x="18" y="25" fill="#FFFFFF" font-size="18" font-weight="700" text-anchor="middle">3</text>
          <text x="50" y="25" fill="#1E293B" font-size="22" font-weight="700">Collect your prints from the tray below</text>
        </g>
      </g>

      <text x="1585" y="975" fill="#64748B" font-size="18" font-weight="500" text-anchor="middle">Your files are private and automatically deleted after printing</text>
    </g>
  </g>
</svg>`,
		gradID,
		slide.title,
		slide.desc,
		dot1W, dot1A,
		dot1W+14, dot2W, dot2A,
		dot1W+14+dot2W+14, dot3W, dot3A,
		qrB64,
	))

	return buf.String()
}

func buildDownloadingSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Downloading Screen", ""))
	buf.WriteString(buildCommonHeader(logoB64))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="DownloadingCard">
    <rect x="30" y="140" width="1860" height="910" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <text x="90" y="225" fill="#2563EB" font-size="56" font-weight="800">Receiving Your Document...</text>
    <text x="90" y="295" fill="#1E293B" font-size="32" font-weight="500">Your file is being sent to the printer. This only takes a few seconds.</text>

    <!-- Detail Pill -->
    <rect x="90" y="360" width="700" height="60" rx="30" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="2"/>
    <text x="120" y="398" fill="#2563EB" font-size="22" font-weight="700">Document: Lecture_Notes_CS101.pdf • Order #883921</text>

    <!-- Progress Bar -->
    <g id="ProgressBar" transform="translate(90, 500)">
      <rect width="1740" height="56" rx="14" fill="#E2E8F0" stroke="#CBD5E1" stroke-width="2"/>
      <rect x="2" y="2" width="1180" height="52" rx="12" fill="#2563EB"/>
    </g>

    <text x="90" y="595" fill="#1E293B" font-size="32" font-weight="700">Getting ready: 68% completed</text>
    <text x="90" y="650" fill="#64748B" font-size="24" font-weight="400">Printing will start automatically as soon as the file finishes loading.</text>
  </g>
</svg>`)
	return buf.String()
}

func buildPrintingSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Printing Screen", ""))
	buf.WriteString(buildCommonHeader(logoB64))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="PrintingCard">
    <rect x="30" y="140" width="1860" height="910" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <text x="90" y="225" fill="#2563EB" font-size="56" font-weight="800">Printing Your Document...</text>
    <text x="90" y="295" fill="#1E293B" font-size="32" font-weight="500">Your pages are printing right now. Please wait a moment...</text>

    <!-- Page Counter Pill -->
    <rect x="90" y="360" width="260" height="60" rx="30" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="2"/>
    <text x="220" y="399" fill="#2563EB" font-size="28" font-weight="700" text-anchor="middle">Page 2 of 5</text>

    <!-- Detail Pill -->
    <rect x="380" y="360" width="560" height="60" rx="30" fill="#F8FAFC" stroke="#E2E8F0" stroke-width="2"/>
    <text x="410" y="398" fill="#64748B" font-size="22" font-weight="600">Order #883921 • Single-sided • 1 Copy</text>

    <!-- Progress Bar -->
    <g id="ProgressBar" transform="translate(90, 500)">
      <rect width="1740" height="56" rx="14" fill="#E2E8F0" stroke="#CBD5E1" stroke-width="2"/>
      <rect x="2" y="2" width="694" height="52" rx="12" fill="#2563EB"/>
    </g>

    <text x="90" y="595" fill="#1E293B" font-size="32" font-weight="700">Printing: 2 of 5 pages completed</text>
  </g>
</svg>`)
	return buf.String()
}

func buildCompletedSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Completed Screen", ""))
	buf.WriteString(buildCommonHeader(logoB64))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="CompletedCard">
    <rect x="30" y="140" width="1860" height="910" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <text x="90" y="225" fill="#10B981" font-size="56" font-weight="800">All Done! Printing Finished</text>
    <text x="90" y="295" fill="#1E293B" font-size="32" font-weight="500">Your document has finished printing successfully.</text>

    <!-- Detail Pill -->
    <rect x="90" y="360" width="460" height="60" rx="30" fill="#ECFDF5" stroke="#A7F3D0" stroke-width="2"/>
    <text x="120" y="398" fill="#047857" font-size="22" font-weight="700">Order #883921 • 5 Sheets Printed</text>

    <!-- Collection Callout Banner -->
    <rect x="90" y="450" width="1740" height="95" rx="24" fill="#ECFDF5" stroke="#A7F3D0" stroke-width="2"/>
    <text x="960" y="508" fill="#047857" font-size="32" font-weight="700" text-anchor="middle">Please take all your printed pages from the tray below</text>

    <!-- Instructions Box -->
    <rect x="90" y="580" width="1740" height="250" rx="20" fill="#F8FAFC" stroke="#E2E8F0" stroke-width="2"/>
    <g id="Instructions" transform="translate(140, 645)">
      <text x="0" y="0" fill="#1E293B" font-size="28" font-weight="600">1. Double check that you've collected all your pages.</text>
      <text x="0" y="55" fill="#1E293B" font-size="28" font-weight="600">2. This screen will reset automatically for the next user.</text>
      <text x="0" y="115" fill="#64748B" font-size="24" font-weight="400">Thank you for using XeroxGo!</text>
    </g>
  </g>
</svg>`)
	return buf.String()
}

func buildErrorSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Error Screen", ""))
	buf.WriteString(buildCommonHeader(logoB64))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="ErrorCard">
    <rect x="30" y="140" width="1860" height="910" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <text x="90" y="225" fill="#EF4444" font-size="56" font-weight="800">Printer Needs Attention</text>
    <text x="90" y="295" fill="#1E293B" font-size="32" font-weight="700">The printer has run out of paper or needs a quick check.</text>

    <!-- Detail Pill -->
    <rect x="90" y="360" width="520" height="60" rx="30" fill="#FEF2F2" stroke="#FECACA" stroke-width="2"/>
    <text x="120" y="398" fill="#DC2626" font-size="22" font-weight="700">Paper tray empty • Order #883921</text>

    <!-- Recovery Box -->
    <g id="RecoveryInstructions" transform="translate(90, 450)">
      <rect width="1740" height="520" rx="20" fill="#FFFBEB" stroke="#FDE68A" stroke-width="2"/>
      <text x="50" y="55" fill="#B45309" font-size="28" font-weight="700">What you can do to fix this:</text>

      <g transform="translate(50, 125)">
        <text x="0" y="0" fill="#1E293B" font-size="26" font-weight="600">1. Pull open the bottom paper tray and load standard A4 paper.</text>
        <text x="0" y="65" fill="#1E293B" font-size="26" font-weight="600">2. If paper is stuck, gently remove it and firmly close the printer cover.</text>
        <text x="0" y="130" fill="#1E293B" font-size="26" font-weight="600">3. Check that the printed output tray is empty and clear.</text>
        <text x="0" y="195" fill="#1E293B" font-size="26" font-weight="600">4. If it still doesn't print, please alert the shop or campus attendant.</text>
      </g>
    </g>
  </g>
</svg>`)
	return buf.String()
}

func buildMasterArtboardSVG(screens []struct {
	filename string
	title    string
	svg      string
}) string {
	const (
		pad = 100
		sw  = 1920
		sh  = 1080
	)
	cols := 3
	rows := (len(screens) + cols - 1) / cols
	totalW := cols*sw + (cols+1)*pad
	totalH := rows*sh + (rows+1)*pad

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg width="%d" height="%d" viewBox="0 0 %d %d" fill="none" xmlns="http://www.w3.org/2000/svg">
  <title>XeroxGo Kiosk - Master UI Design Board</title>
  <defs>
    <style>
      @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&amp;display=swap');
      text { font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
    </style>
  </defs>
  <rect width="%d" height="%d" fill="#0F172A"/>
`, totalW, totalH, totalW, totalH, totalW, totalH))

	for i, s := range screens {
		r := i / cols
		c := i % cols
		x := pad + c*(sw+pad)
		y := pad + r*(sh+pad)

		buf.WriteString(fmt.Sprintf(`
  <g id="ScreenFrame_%d" transform="translate(%d, %d)">
    <text x="0" y="-30" fill="#94A3B8" font-size="28" font-weight="700">%s (%s)</text>
    <rect width="%d" height="%d" rx="28" fill="#F8FAFC" filter="drop-shadow(0 20px 40px rgba(0,0,0,0.5))"/>
    <g transform="translate(0, 0)">
`, i+1, x, y, s.title, s.filename, sw, sh))

		// Extract inner svg content
		content := s.svg
		startIdx := bytes.Index([]byte(content), []byte("<!-- Background Slate-50 -->"))
		endIdx := bytes.LastIndex([]byte(content), []byte("</svg>"))
		if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
			buf.WriteString(content[startIdx:endIdx])
		}

		buf.WriteString(`
    </g>
  </g>
`)
	}

	buf.WriteString("</svg>\n")
	return buf.String()
}
