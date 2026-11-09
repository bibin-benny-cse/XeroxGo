package input

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestDevKeyboardAdapter_PINSubmissionPreservesLeadingZeros(t *testing.T) {
	var submittedPIN string
	var submitCount int

	ctrl := NewController(Config{
		OnSubmit: func(ctx context.Context, pin string) error {
			submittedPIN = pin
			submitCount++
			return nil
		},
	})

	inputData := "0482\n"
	inBuf := strings.NewReader(inputData)
	outBuf := &bytes.Buffer{}

	adapter := NewDevKeyboardAdapter(ctrl, inBuf, outBuf)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := adapter.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected adapter error: %v", err)
	}

	if submitCount != 1 {
		t.Fatalf("expected 1 submission, got %d", submitCount)
	}
	if submittedPIN != "0482" {
		t.Fatalf("expected PIN '0482' with preserved leading zero, got %q", submittedPIN)
	}
}

func TestDevKeyboardAdapter_CancelAndCommands(t *testing.T) {
	var canceled bool
	var submittedPIN string

	ctrl := NewController(Config{
		OnCancel: func() {
			canceled = true
		},
		OnSubmit: func(ctx context.Context, pin string) error {
			submittedPIN = pin
			return nil
		},
	})

	inputData := "help\ncancel\n1234\n"
	inBuf := strings.NewReader(inputData)
	outBuf := &bytes.Buffer{}

	adapter := NewDevKeyboardAdapter(ctrl, inBuf, outBuf)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := adapter.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected adapter error: %v", err)
	}

	if !canceled {
		t.Error("expected cancel to have been invoked")
	}
	if submittedPIN != "1234" {
		t.Fatalf("expected PIN '1234', got %q", submittedPIN)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, "XEROXGO DEVELOPMENT KEYBOARD COMMANDS") {
		t.Error("expected help output to be displayed")
	}
}

func TestDevKeyboardAdapter_SingleDigitEntry(t *testing.T) {
	var submittedPIN string

	ctrl := NewController(Config{
		OnSubmit: func(ctx context.Context, pin string) error {
			submittedPIN = pin
			return nil
		},
	})

	// Enter 0, 7, 8, 9 one by one
	inputData := "0\n7\n8\n9\n"
	inBuf := strings.NewReader(inputData)
	outBuf := &bytes.Buffer{}

	adapter := NewDevKeyboardAdapter(ctrl, inBuf, outBuf)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := adapter.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected adapter error: %v", err)
	}

	if submittedPIN != "0789" {
		t.Fatalf("expected PIN '0789', got %q", submittedPIN)
	}
}

func TestDevKeyboardAdapter_InvalidInputIgnoredGracefully(t *testing.T) {
	var submittedPIN string

	ctrl := NewController(Config{
		OnSubmit: func(ctx context.Context, pin string) error {
			submittedPIN = pin
			return nil
		},
	})

	inputData := "12\nabc\n12345\n9876\n"
	inBuf := strings.NewReader(inputData)
	outBuf := &bytes.Buffer{}

	adapter := NewDevKeyboardAdapter(ctrl, inBuf, outBuf)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := adapter.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected adapter error: %v", err)
	}

	if submittedPIN != "9876" {
		t.Fatalf("expected PIN '9876', got %q", submittedPIN)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, "Unrecognized command or invalid PIN") {
		t.Error("expected error message for invalid inputs")
	}
}
