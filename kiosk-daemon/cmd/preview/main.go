package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

func main() {
	outDir := "previews"
	if err := os.MkdirAll(outDir, 0755); err != nil {
		panic(err)
	}

	states := []struct {
		name  string
		state ui_ipc.ScreenState
	}{
		{
			name: "01_idle.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeIdle,
				QRContent:    "https://xeroxgo-frontend.vercel.app",
				PrinterModel: "Brother DCP-T310",
				AdSlideIndex: 0,
			},
		},
		{
			name: "01_idle_slide2.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeIdle,
				QRContent:    "https://xeroxgo-frontend.vercel.app",
				PrinterModel: "Brother DCP-T310",
				AdSlideIndex: 1,
			},
		},
		{
			name: "01_idle_slide3.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeIdle,
				QRContent:    "https://xeroxgo-frontend.vercel.app",
				PrinterModel: "Brother DCP-T310",
				AdSlideIndex: 2,
			},
		},
		{
			name: "01_idle_transition.png",
			state: ui_ipc.ScreenState{
				Mode:           ui_ipc.ModeIdle,
				QRContent:      "https://xeroxgo-frontend.vercel.app",
				PrinterModel:   "Brother DCP-T310",
				AdSlideIndex:   1,
				TransitionFrom: 0,
				TransitionProg: 0.5,
			},
		},
		{
			name: "02_downloading.png",
			state: ui_ipc.ScreenState{
				Mode:            ui_ipc.ModeDownloading,
				ProgressPercent: 68,
				Message:         "Receiving print job securely from cloud server...",
				Detail:          "Job ID: xg-order-883921 • Document: Lecture_Notes_CS101.pdf",
				PrinterModel:    "Brother DCP-T310",
			},
		},
		{
			name: "03_printing.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModePrinting,
				CurrentPage:  2,
				TotalPages:   5,
				Message:      "Please wait while your document prints...",
				Detail:       "Job ID: xg-order-883921 • Duplex Simplex • 1 Copy",
				PrinterModel: "Brother DCP-T310",
			},
		},
		{
			name: "04_completed.png",
			state: ui_ipc.ScreenState{
				Mode:            ui_ipc.ModeCompleted,
				CurrentPage:     5,
				TotalPages:      5,
				ProgressPercent: 100,
				Message:         "All 5 pages have been streamed and printed successfully.",
				Detail:          "Job ID: xg-order-883921 • Total Sheets: 5",
				PrinterModel:    "Brother DCP-T310",
			},
		},
		{
			name: "05_error.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeError,
				Message:      "Printer paper tray 1 is empty or disconnected.",
				Detail:       "Error Code: CUPS_MEDIA_EMPTY • Job ID: xg-order-883921",
				PrinterModel: "Brother DCP-T310",
			},
		},
	}

	var idleScreen image.Image
	for _, s := range states {
		img, err := ui_ipc.Render(s.state)
		if err != nil {
			panic(fmt.Sprintf("render %s failed: %v", s.name, err))
		}
		if s.name == "01_idle.png" {
			idleScreen = img
		}
		path := filepath.Join(outDir, s.name)
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, img); err != nil {
			panic(err)
		}
		_ = f.Close()
		fmt.Printf("Generated %s\n", path)
	}

	// Export individual standalone cards for design inspection
	cards := []struct {
		name string
		img  image.Image
	}{
		{"card_ad_slide1.png", ui_ipc.GetAdSlideImage(0)},
		{"card_ad_slide2.png", ui_ipc.GetAdSlideImage(1)},
		{"card_ad_slide3.png", ui_ipc.GetAdSlideImage(2)},
	}

	if idleScreen != nil {
		type subImager interface {
			SubImage(r image.Rectangle) image.Image
		}
		if si, ok := idleScreen.(subImager); ok {
			qrCard := si.SubImage(image.Rect(1268, 140, 1891, 980))
			cards = append(cards, struct {
				name string
				img  image.Image
			}{"card_qr_portal.png", qrCard})
		}
	}

	for _, c := range cards {
		if c.img == nil {
			continue
		}
		path := filepath.Join(outDir, c.name)
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, c.img); err != nil {
			panic(err)
		}
		_ = f.Close()
		fmt.Printf("Generated card: %s\n", path)
	}
}
