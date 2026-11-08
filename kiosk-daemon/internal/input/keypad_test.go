package input

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestKeypadController_PreservesLeadingZeros(t *testing.T) {
	var submittedPIN string
	var updates []string
	var mu sync.Mutex

	ctrl := NewController(Config{
		OnChange: func(current string) {
			mu.Lock()
			updates = append(updates, current)
			mu.Unlock()
		},
		OnSubmit: func(ctx context.Context, pin string) error {
			mu.Lock()
			submittedPIN = pin
			mu.Unlock()
			return nil
		},
	})

	ctx := context.Background()

	// Type digits '0', '4', '8', '2'
	for _, r := range "0482" {
		if err := ctrl.HandleRune(ctx, r); err != nil {
			t.Fatalf("HandleRune(%c) failed: %v", r, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if submittedPIN != "0482" {
		t.Fatalf("expected submitted PIN to be %q, got %q", "0482", submittedPIN)
	}

	expectedUpdates := []string{"0", "04", "048", "0482"}
	if len(updates) != len(expectedUpdates) {
		t.Fatalf("expected %d updates, got %d: %v", len(expectedUpdates), len(updates), updates)
	}
	for i, exp := range expectedUpdates {
		if updates[i] != exp {
			t.Fatalf("update[%d]: got %q, want %q", i, updates[i], exp)
		}
	}
}

func TestKeypadController_BackspaceAndCorrection(t *testing.T) {
	var submittedPIN string
	ctrl := NewController(Config{
		OnSubmit: func(ctx context.Context, pin string) error {
			submittedPIN = pin
			return nil
		},
	})

	ctx := context.Background()

	// Type '0', '4', '9'
	_ = ctrl.HandleRune(ctx, '0')
	_ = ctrl.HandleRune(ctx, '4')
	_ = ctrl.HandleRune(ctx, '9')
	if ctrl.CurrentPIN() != "049" {
		t.Fatalf("expected %q, got %q", "049", ctrl.CurrentPIN())
	}

	// Hit backspace
	_ = ctrl.HandleRune(ctx, '\b')
	if ctrl.CurrentPIN() != "04" {
		t.Fatalf("after backspace: expected %q, got %q", "04", ctrl.CurrentPIN())
	}

	// Type '8', '2'
	_ = ctrl.HandleRune(ctx, '8')
	_ = ctrl.HandleRune(ctx, '2')

	if submittedPIN != "0482" {
		t.Fatalf("expected submitted PIN %q, got %q", "0482", submittedPIN)
	}
}

func TestKeypadController_ResetAndCancel(t *testing.T) {
	canceled := false
	ctrl := NewController(Config{
		OnCancel: func() {
			canceled = true
		},
	})

	ctx := context.Background()
	_ = ctrl.HandleRune(ctx, '0')
	_ = ctrl.HandleRune(ctx, '4')
	if ctrl.CurrentPIN() != "04" {
		t.Fatalf("expected %q, got %q", "04", ctrl.CurrentPIN())
	}

	// Hit Escape
	_ = ctrl.HandleRune(ctx, '\x1b')
	if !canceled {
		t.Fatalf("expected onCancel to be called")
	}
	if ctrl.CurrentPIN() != "" {
		t.Fatalf("expected empty buffer after reset, got %q", ctrl.CurrentPIN())
	}
}

func TestKeypadController_ReadLoop(t *testing.T) {
	submittedChan := make(chan string, 1)
	ctrl := NewController(Config{
		OnSubmit: func(ctx context.Context, pin string) error {
			submittedChan <- pin
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	inputReader := strings.NewReader("0482")
	go func() {
		_ = ctrl.ReadLoop(ctx, inputReader)
	}()

	select {
	case pin := <-submittedChan:
		if pin != "0482" {
			t.Fatalf("expected %q, got %q", "0482", pin)
		}
	case <-ctx.Done():
		t.Fatalf("ReadLoop timed out waiting for PIN submission")
	}
}
