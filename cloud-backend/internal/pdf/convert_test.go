package pdf

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIsSupportedFormat(t *testing.T) {
	valid := []string{
		"doc.pdf", "file.PDF",
		"notes.doc", "notes.docx",
		"slides.ppt", "slides.pptx",
		"sheet.xls", "sheet.xlsx",
		"doc.odt", "sheet.ods", "slides.odp",
		"letter.rtf", "memo.txt",
		"photo.jpg", "photo.jpeg", "logo.png",
		"scan.bmp", "hero.webp", "drawing.tiff",
	}

	for _, name := range valid {
		if !IsSupportedFormat(name) {
			t.Errorf("expected %q to be supported", name)
		}
	}

	invalid := []string{
		"virus.exe", "bundle.zip", "page.html",
		"icon.svg", "script.sh", "file.raw",
		"document",
	}

	for _, name := range invalid {
		if IsSupportedFormat(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

func TestConvertToPDF_Validation(t *testing.T) {
	ctx := context.Background()

	if err := ConvertToPDF(nil, "in", "in.txt", "out.pdf"); err == nil {
		t.Error("expected error on nil context")
	}

	if err := ConvertToPDF(ctx, "", "in.txt", "out.pdf"); err == nil {
		t.Error("expected error on empty input path")
	}

	if err := ConvertToPDF(ctx, "in.txt", "in.txt", ""); err == nil {
		t.Error("expected error on empty output path")
	}

	if err := ConvertToPDF(ctx, "in.exe", "in.exe", "out.pdf"); err == nil {
		t.Error("expected error on unsupported extension")
	}
}

func TestConvertToPDF_TextAndImage(t *testing.T) {
	if _, err := exec.LookPath("soffice"); err != nil {
		t.Skip("soffice (LibreOffice) not available in PATH; skipping conversion integration test")
	}

	tmpDir := t.TempDir()

	// 1. Convert Text Document (.txt)
	txtInput := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(txtInput, []byte("XeroxGo Automated Document Conversion Test\nPage 1"), 0644); err != nil {
		t.Fatalf("failed to create sample text file: %v", err)
	}
	txtOutput := filepath.Join(tmpDir, "sample_txt.pdf")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := ConvertToPDF(ctx, txtInput, "sample.txt", txtOutput); err != nil {
		t.Fatalf("ConvertToPDF(.txt) failed: %v", err)
	}

	info, err := os.Stat(txtOutput)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty output PDF for text file: %v", err)
	}

	// 2. Convert PNG Image (.png)
	pngInput := filepath.Join(tmpDir, "test_image.png")
	img := image.NewRGBA(image.Rect(0, 0, 150, 150))
	for y := 0; y < 150; y++ {
		for x := 0; x < 150; x++ {
			img.Set(x, y, color.RGBA{R: 50, G: 120, B: 220, A: 255})
		}
	}
	f, err := os.Create(pngInput)
	if err != nil {
		t.Fatalf("failed to create png file: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("failed to encode png: %v", err)
	}
	f.Close()

	pngOutput := filepath.Join(tmpDir, "sample_png.pdf")
	if err := ConvertToPDF(ctx, pngInput, "test_image.png", pngOutput); err != nil {
		t.Fatalf("ConvertToPDF(.png) failed: %v", err)
	}

	infoPng, err := os.Stat(pngOutput)
	if err != nil || infoPng.Size() == 0 {
		t.Fatalf("expected non-empty output PDF for image file: %v", err)
	}

	// 3. Passthrough PDF
	pdfPassthrough := filepath.Join(tmpDir, "passthrough.pdf")
	if err := ConvertToPDF(ctx, txtOutput, "sample_txt.pdf", pdfPassthrough); err != nil {
		t.Fatalf("ConvertToPDF(.pdf passthrough) failed: %v", err)
	}
	if infoPass, err := os.Stat(pdfPassthrough); err != nil || infoPass.Size() != info.Size() {
		t.Fatalf("expected passthrough PDF to match original size: %v", err)
	}
}
