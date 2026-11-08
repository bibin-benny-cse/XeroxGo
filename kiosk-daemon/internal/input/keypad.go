package input

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"unicode"
)

// PINSubmitHandler is called when a complete 4-digit PIN is entered and submitted.
type PINSubmitHandler func(ctx context.Context, pin string) error

// PINChangeHandler is called when digits are typed or deleted, for instant UI reflection.
type PINChangeHandler func(currentPIN string)

// CancelHandler is called when the user presses Cancel/Escape to reset to Idle.
type CancelHandler func()

// Controller handles physical keyboard, numeric keypad, or touchscreen input on the kiosk.
// It maintains an in-memory digit buffer that strictly preserves leading zeros (e.g. "0482").
type Controller struct {
	mu        sync.Mutex
	pinBuffer strings.Builder
	onSubmit  PINSubmitHandler
	onChange  PINChangeHandler
	onCancel  CancelHandler
	isBusy    bool
}

// Config configures the input controller callbacks.
type Config struct {
	OnSubmit PINSubmitHandler
	OnChange PINChangeHandler
	OnCancel CancelHandler
}

// NewController creates an input controller for customer PIN entry.
func NewController(cfg Config) *Controller {
	return &Controller{
		onSubmit: cfg.OnSubmit,
		onChange: cfg.OnChange,
		onCancel: cfg.OnCancel,
	}
}

// CurrentPIN returns the currently buffered PIN string.
func (c *Controller) CurrentPIN() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinBuffer.String()
}

// Reset clears the PIN buffer and notifies the cancel handler.
func (c *Controller) Reset() {
	c.mu.Lock()
	c.pinBuffer.Reset()
	c.isBusy = false
	c.mu.Unlock()

	if c.onCancel != nil {
		c.onCancel()
	}
}

// HandleRune processes a single rune from a keyboard, numeric keypad, or touch button.
// It preserves leading zeros and auto-submits when exactly 4 digits are accumulated.
func (c *Controller) HandleRune(ctx context.Context, r rune) error {
	c.mu.Lock()
	if c.isBusy {
		c.mu.Unlock()
		return errors.New("controller is busy processing previous submission")
	}

	switch {
	case unicode.IsDigit(r):
		if c.pinBuffer.Len() < 4 {
			c.pinBuffer.WriteRune(r)
			current := c.pinBuffer.String()
			c.mu.Unlock()

			if c.onChange != nil {
				c.onChange(current)
			}

			// Auto-submit once 4 digits have been entered
			if len(current) == 4 {
				return c.Submit(ctx)
			}
			return nil
		}
		c.mu.Unlock()
		return nil

	case r == '\b' || r == 127: // Backspace or Delete
		if c.pinBuffer.Len() > 0 {
			str := c.pinBuffer.String()
			c.pinBuffer.Reset()
			c.pinBuffer.WriteString(str[:len(str)-1])
			current := c.pinBuffer.String()
			c.mu.Unlock()

			if c.onChange != nil {
				c.onChange(current)
			}
			return nil
		}
		c.mu.Unlock()
		return nil

	case r == '\x1b': // Escape / Cancel
		c.mu.Unlock()
		c.Reset()
		return nil

	case r == '\r' || r == '\n': // Enter key
		if c.pinBuffer.Len() == 4 {
			c.mu.Unlock()
			return c.Submit(ctx)
		}
		c.mu.Unlock()
		return nil

	default:
		c.mu.Unlock()
		return nil
	}
}

// Submit validates that the buffer holds exactly 4 numeric digits and invokes the submit handler.
func (c *Controller) Submit(ctx context.Context) error {
	c.mu.Lock()
	if c.isBusy {
		c.mu.Unlock()
		return errors.New("submission already in progress")
	}
	pin := c.pinBuffer.String()
	if len(pin) != 4 {
		c.mu.Unlock()
		return errors.New("cannot submit: PIN must be exactly 4 digits")
	}
	c.isBusy = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isBusy = false
		c.mu.Unlock()
	}()

	if c.onSubmit != nil {
		return c.onSubmit(ctx, pin)
	}
	return nil
}

// ReadLoop reads runes continuously from an io.Reader (such as os.Stdin or a keypad device stream).
func (c *Controller) ReadLoop(ctx context.Context, r io.Reader) error {
	reader := bufio.NewReader(r)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		rn, _, err := reader.ReadRune()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

		_ = c.HandleRune(ctx, rn)
	}
}
