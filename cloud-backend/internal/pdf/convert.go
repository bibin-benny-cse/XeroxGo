package pdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	supportedOfficeExts = map[string]bool{
		".doc":  true,
		".docx": true,
		".ppt":  true,
		".pptx": true,
		".xls":  true,
		".xlsx": true,
		".odt":  true,
		".ods":  true,
		".odp":  true,
		".rtf":  true,
		".txt":  true,
	}

	supportedImageExts = map[string]bool{
		".jpg":  true,
		".jpeg": true,
		".png":  true,
		".bmp":  true,
		".webp": true,
		".tif":  true,
		".tiff": true,
	}

	// libreOfficeSem limits concurrent LibreOffice processes.
	// Initialized to capacity 1 to prevent memory exhaustion on low-memory servers (e.g. Render 512MB).
	libreOfficeSem = make(chan struct{}, 1)
)

// IsSupportedFormat returns true if the filename extension is an accepted format (PDF, Office, or Image).
func IsSupportedFormat(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".pdf" {
		return true
	}
	return supportedOfficeExts[ext] || supportedImageExts[ext]
}

// IsOfficeDocument returns true if the filename has a supported Office document extension.
func IsOfficeDocument(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return supportedOfficeExts[ext]
}

// IsImage returns true if the filename has a supported image extension.
func IsImage(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return supportedImageExts[ext]
}

// ConvertToPDF converts an input file (Office document, image, or text) to a standard PDF.
// If the input file is already a PDF, it copies the input to outputPath directly.
// Uses headless LibreOffice with an isolated profile directory to support thread-safe concurrent conversions.
func ConvertToPDF(ctx context.Context, inputPath, originalFilename, outputPath string) error {
	if ctx == nil {
		return errors.New("context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if inputPath == "" {
		return errors.New("inputPath must not be empty")
	}
	if outputPath == "" {
		return errors.New("outputPath must not be empty")
	}

	ext := strings.ToLower(filepath.Ext(originalFilename))
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(inputPath))
	}

	// 1. If already PDF, copy to outputPath if paths differ
	if ext == ".pdf" {
		if filepath.Clean(inputPath) == filepath.Clean(outputPath) {
			return nil
		}
		return copyFile(inputPath, outputPath)
	}

	// 2. Validate extension per specification
	if !supportedOfficeExts[ext] && !supportedImageExts[ext] {
		return fmt.Errorf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX, ODT, RTF) and Images (JPG, PNG, WEBP) are supported", ext)
	}

	// Limit concurrent LibreOffice processes to 1 to preserve memory on low-memory servers (e.g. Render)
	select {
	case libreOfficeSem <- struct{}{}:
		defer func() { <-libreOfficeSem }()
	case <-ctx.Done():
		return fmt.Errorf("waiting for conversion worker: %w", ctx.Err())
	}

	// 3. Create isolated working directory and user profile
	workDir, err := os.MkdirTemp("", "xeroxgo_convert_*")
	if err != nil {
		return fmt.Errorf("failed to create conversion workdir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(workDir)
	}()

	profileDir := filepath.Join(workDir, "profile")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		return fmt.Errorf("failed to create conversion profile dir: %w", err)
	}

	// Symlink or copy input file to workDir with proper extension so LibreOffice detects filter
	stagedInput := filepath.Join(workDir, "source"+ext)
	if err := copyFile(inputPath, stagedInput); err != nil {
		return fmt.Errorf("failed to stage input file for conversion: %w", err)
	}

	// 4. Execute LibreOffice conversion with timeout
	convCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(convCtx, "soffice",
		"--headless",
		"--invisible",
		"--nodefault",
		"--nofirststartwizard",
		"--nolockcheck",
		"--nologo",
		fmt.Sprintf("-env:UserInstallation=file://%s", profileDir),
		"--convert-to", "pdf",
		"--outdir", workDir,
		stagedInput,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if convCtx.Err() != nil {
			return fmt.Errorf("conversion timed out or cancelled: %w", convCtx.Err())
		}
		return fmt.Errorf("libreoffice conversion failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 5. Resulting PDF is named "source.pdf" in workDir
	generatedPDF := filepath.Join(workDir, "source.pdf")
	if _, err := os.Stat(generatedPDF); err != nil {
		return fmt.Errorf("conversion output not found at %s: %w", generatedPDF, err)
	}

	// 6. Copy to destination outputPath
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	return copyFile(generatedPDF, outputPath)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

