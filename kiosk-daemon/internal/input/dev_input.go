package input

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
)

// DevKeyboardAdapter provides an interactive laptop keyboard interface for development mode.
// It accepts 4-digit pickup PINs from standard input (preserving leading zeros, e.g. "0482"),
// submits them to the input controller, and supports interactive commands such as 'cancel' and 'help'.
type DevKeyboardAdapter struct {
	ctrl   *Controller
	reader io.Reader
	writer io.Writer
	mu     sync.Mutex
}

// NewDevKeyboardAdapter creates a development keyboard adapter bound to an input controller.
func NewDevKeyboardAdapter(ctrl *Controller, r io.Reader, w io.Writer) *DevKeyboardAdapter {
	if w == nil {
		w = io.Discard
	}
	return &DevKeyboardAdapter{
		ctrl:   ctrl,
		reader: r,
		writer: w,
	}
}

// Run executes the input loop, reading lines from the configured reader until ctx is canceled or EOF.
func (a *DevKeyboardAdapter) Run(ctx context.Context) error {
	if a.ctrl == nil {
		return errors.New("dev keyboard adapter: input controller cannot be nil")
	}
	if a.reader == nil {
		return errors.New("dev keyboard adapter: input reader cannot be nil")
	}

	a.prompt()

	scanner := bufio.NewScanner(a.reader)
	lineCh := make(chan string)

	go func() {
		defer close(lineCh)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lineCh:
			if !ok {
				if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
					return fmt.Errorf("read error: %w", err)
				}
				return nil
			}

			if err := a.HandleLine(ctx, line); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
			}
			a.prompt()
		}
	}
}

// HandleLine processes a single command line or numeric PIN from the developer's keyboard.
func (a *DevKeyboardAdapter) HandleLine(ctx context.Context, rawLine string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	line := strings.TrimSpace(rawLine)
	if line == "" {
		return nil
	}

	switch strings.ToLower(line) {
	case "help", "h", "?":
		a.printHelp()
		return nil

	case "cancel", "c", "esc", "reset":
		a.ctrl.Reset()
		fmt.Fprintln(a.writer, "[DEV KEYBOARD] Reset to Idle screen.")
		return nil

	case "exit", "quit", "q":
		fmt.Fprintln(a.writer, "[DEV KEYBOARD] Exiting development mode.")
		return context.Canceled

	default:
		// Check if it is a 4-digit pickup PIN
		if len(line) == 4 && isAllDigits(line) {
			// Clear buffer and enter runes into controller
			a.ctrl.Reset()
			for _, r := range line {
				if err := a.ctrl.HandleRune(ctx, r); err != nil {
					fmt.Fprintf(a.writer, "[DEV KEYBOARD] Error entering digit %c: %v\n", r, err)
					return err
				}
			}
			fmt.Fprintf(a.writer, "[DEV KEYBOARD] PIN entered: %s\n", line)
			return nil
		}

		// Single digit entry (e.g. step-by-step keypad emulation)
		if len(line) == 1 && unicode.IsDigit(rune(line[0])) {
			r := rune(line[0])
			if err := a.ctrl.HandleRune(ctx, r); err != nil {
				fmt.Fprintf(a.writer, "[DEV KEYBOARD] Error entering digit %c: %v\n", r, err)
				return err
			}
			fmt.Fprintf(a.writer, "[DEV KEYBOARD] Digit accepted. Current buffer: %s\n", a.ctrl.CurrentPIN())
			return nil
		}

		fmt.Fprintf(a.writer, "[DEV KEYBOARD] Unrecognized command or invalid PIN: %q (Type a 4-digit PIN like '0482', or 'cancel' / 'help')\n", line)
		return nil
	}
}

func (a *DevKeyboardAdapter) prompt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprint(a.writer, "[DEV KEYBOARD] Enter 4-digit PIN (or 'cancel' / 'help') > ")
}

func (a *DevKeyboardAdapter) printHelp() {
	fmt.Fprintln(a.writer, "\n================================================================================")
	fmt.Fprintln(a.writer, "                     XEROXGO DEVELOPMENT KEYBOARD COMMANDS                      ")
	fmt.Fprintln(a.writer, "================================================================================")
	fmt.Fprintln(a.writer, "  <4-digit PIN>  : Enter customer pickup PIN (e.g. '0482', '1234')")
	fmt.Fprintln(a.writer, "  <single digit> : Enter digit one-by-one (auto-submits when 4 digits reached)")
	fmt.Fprintln(a.writer, "  cancel / c     : Reset active workflow and return kiosk to Idle screen")
	fmt.Fprintln(a.writer, "  help / h       : Display this help guide")
	fmt.Fprintln(a.writer, "  exit / q       : Stop the development kiosk daemon")
	fmt.Fprintln(a.writer, "================================================================================")
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
