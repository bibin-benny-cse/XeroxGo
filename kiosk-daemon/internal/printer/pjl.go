package printer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"

	"xeroxgo/pkg/contracts"
)

const universalExitLanguage = "\x1b%-12345X"

type closeWriter interface {
	CloseWrite() error
}

func sanitizePJLJobName(jobID string) string {
	var b strings.Builder
	for i := 0; i < len(jobID); i++ {
		c := jobID[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
		if b.Len() == 64 {
			break
		}
	}
	res := b.String()
	if res == "" {
		return "xeroxgo"
	}
	return res
}

func validatePJLCommand(
	ctx context.Context,
	addr string,
	cmd contracts.PrintJobCommand,
	pdfReader io.Reader,
) error {
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if addr == "" {
		return errors.New("address cannot be empty")
	}
	if pdfReader == nil {
		return errors.New("pdfReader cannot be nil")
	}
	if cmd.JobID == "" {
		return errors.New("job_id cannot be empty")
	}
	if cmd.Copies < 1 || cmd.Copies > 999 {
		return fmt.Errorf("copies must be between 1 and 999, got %d", cmd.Copies)
	}
	if cmd.TotalPages < 1 {
		return fmt.Errorf("total_pages must be >= 1, got %d", cmd.TotalPages)
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("total_sheets must be >= 1, got %d", cmd.TotalSheets)
	}
	if cmd.StartFromPage < 1 {
		return fmt.Errorf("start_from_page must be >= 1, got %d", cmd.StartFromPage)
	}
	if cmd.StartFromPage > cmd.TotalPages {
		return fmt.Errorf("start_from_page (%d) cannot exceed total_pages (%d)", cmd.StartFromPage, cmd.TotalPages)
	}

	switch cmd.Duplex {
	case contracts.DuplexSimplex, contracts.DuplexLongEdge, contracts.DuplexShortEdge:
	default:
		return fmt.Errorf("invalid duplex mode: %q", cmd.Duplex)
	}

	return nil
}

func buildPJLHeader(cmd contracts.PrintJobCommand) []byte {
	safeName := sanitizePJLJobName(cmd.JobID)
	var b strings.Builder
	b.WriteString(universalExitLanguage)
	b.WriteString("@PJL JOB NAME=\"")
	b.WriteString(safeName)
	b.WriteString("\"\r\n")
	b.WriteString("@PJL USTATUS DEVICE=ON\r\n")
	b.WriteString("@PJL USTATUS JOB=ON\r\n")
	b.WriteString("@PJL USTATUS PAGE=ON\r\n")
	fmt.Fprintf(&b, "@PJL SET COPIES=%d\r\n", cmd.Copies)

	switch cmd.Duplex {
	case contracts.DuplexSimplex:
		b.WriteString("@PJL SET DUPLEX=OFF\r\n")
	case contracts.DuplexLongEdge:
		b.WriteString("@PJL SET DUPLEX=ON\r\n")
		b.WriteString("@PJL SET BINDING=LONGEDGE\r\n")
	case contracts.DuplexShortEdge:
		b.WriteString("@PJL SET DUPLEX=ON\r\n")
		b.WriteString("@PJL SET BINDING=SHORTEDGE\r\n")
	}

	b.WriteString("@PJL ENTER LANGUAGE=PDF\r\n")
	return []byte(b.String())
}

func parsePJLStatus(
	ctx context.Context,
	conn net.Conn,
	writeCompleted *atomic.Bool,
	onPage func(sheetsCompleted int),
) error {
	reader := bufio.NewReader(conn)
	var lineBuf bytes.Buffer
	const maxLineLen = 4096

	type parserState int
	const (
		stateNormal parserState = iota
		stateExpectPageNum
		stateExpectJobStatus
	)

	state := stateNormal
	lastPage := 0
	gotJobEnd := false

	processLine := func(line string) bool {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return false
		}

		switch state {
		case stateNormal:
			if len(fields) >= 3 && strings.EqualFold(fields[0], "@PJL") && strings.EqualFold(fields[1], "USTATUS") {
				if strings.EqualFold(fields[2], "PAGE") {
					state = stateExpectPageNum
				} else if strings.EqualFold(fields[2], "JOB") {
					state = stateExpectJobStatus
				}
			}
		case stateExpectPageNum:
			state = stateNormal
			if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 && n > lastPage {
				lastPage = n
				if onPage != nil {
					onPage(n)
				}
			}
		case stateExpectJobStatus:
			state = stateNormal
			if strings.EqualFold(fields[0], "END") {
				gotJobEnd = true
				if writeCompleted.Load() {
					return true
				}
			}
		}
		return false
	}

	for {
		b, err := reader.ReadByte()
		if err != nil {
			if lineBuf.Len() > 0 {
				line := strings.TrimSpace(lineBuf.String())
				lineBuf.Reset()
				if len(line) > 0 {
					if processLine(line) {
						return nil
					}
				}
			}

			if err == io.EOF {
				if writeCompleted.Load() || gotJobEnd {
					return nil
				}
				return io.ErrUnexpectedEOF
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		if b == '\n' || b == '\f' {
			line := strings.TrimSpace(lineBuf.String())
			lineBuf.Reset()
			if len(line) > 0 {
				if processLine(line) {
					return nil
				}
			}
			if gotJobEnd && writeCompleted.Load() {
				return nil
			}
		} else if b == '\r' {
			continue
		} else {
			if lineBuf.Len() >= maxLineLen {
				return errors.New("status line exceeds maximum length of 4096 bytes")
			}
			lineBuf.WriteByte(b)
		}
	}
}

func StreamPDFWithPJL(
	ctx context.Context,
	addr string,
	cmd contracts.PrintJobCommand,
	pdfReader io.Reader,
	onPage func(sheetsCompleted int),
) error {
	if err := validatePJLCommand(ctx, addr, cmd, pdfReader); err != nil {
		return err
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect to printer at %s: %w", addr, err)
	}
	defer conn.Close()

	stopWatcher := make(chan struct{})
	defer close(stopWatcher)

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopWatcher:
		}
	}()

	var writeCompleted atomic.Bool
	readErrCh := make(chan error, 1)

	// Start reading printer status concurrently BEFORE transmitting header
	go func() {
		readErrCh <- parsePJLStatus(ctx, conn, &writeCompleted, onPage)
	}()

	// Send PJL Header
	header := buildPJLHeader(cmd)
	if _, err := conn.Write(header); err != nil {
		_ = conn.Close()
		<-readErrCh
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("failed to write PJL header: %w", err)
	}

	// Stream PDF payload directly
	if _, err := io.Copy(conn, pdfReader); err != nil {
		_ = conn.Close()
		<-readErrCh
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("failed to stream PDF payload: %w", err)
	}

	// Send PJL Trailer
	safeName := sanitizePJLJobName(cmd.JobID)
	trailer := universalExitLanguage +
		"@PJL EOJ NAME=\"" + safeName + "\"\r\n" +
		universalExitLanguage
	if _, err := conn.Write([]byte(trailer)); err != nil {
		_ = conn.Close()
		<-readErrCh
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("failed to write PJL trailer: %w", err)
	}

	// Signal write completion via CloseWrite
	if cw, ok := conn.(closeWriter); ok {
		if err := cw.CloseWrite(); err != nil {
			_ = conn.Close()
			<-readErrCh
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("failed to close write side of connection: %w", err)
		}
	}
	writeCompleted.Store(true)

	// Wait for status reader to finish
	err = <-readErrCh
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

	return nil
}
