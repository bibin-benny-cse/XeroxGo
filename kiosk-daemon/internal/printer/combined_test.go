package printer

import (
	"context"
	"strings"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestCombineHealth(t *testing.T) {
	t.Run("both_online_idle_and_healthy", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 150,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   150,
			TonerLevelPercent: 82,
			DrumLifePercent:   90,
			CoverOpen:         false,
			PaperJam:          false,
			OutputBinFull:     false,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if !res.Online {
			t.Error("expected Online = true")
		}
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", res.State)
		}
		if res.TonerLevelPercent != 82 {
			t.Errorf("TonerLevelPercent = %d, want 82", res.TonerLevelPercent)
		}
		if res.DrumLifePercent != 90 {
			t.Errorf("DrumLifePercent = %d, want 90", res.DrumLifePercent)
		}
		if res.PaperTrayStatus != "OK" {
			t.Errorf("PaperTrayStatus = %s, want OK", res.PaperTrayStatus)
		}
		if res.ErrorMessage != "" {
			t.Errorf("expected empty ErrorMessage, got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_printing_state_authoritative_over_snmp_idle", func(t *testing.T) {
		// Requirement 13: Never overwrite a confirmed CUPS printing state with an unsupported SNMP status.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 145,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   145,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "PRINTING" {
			t.Errorf("State = %s, want PRINTING (CUPS authoritative)", res.State)
		}
		if res.TonerLevelPercent != 80 {
			t.Errorf("TonerLevelPercent = %d, want 80", res.TonerLevelPercent)
		}
	})

	t.Run("cups_printing_with_snmp_hardware_jam_elevates_error_info", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 140,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 140,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "PRINTING" {
			t.Errorf("State = %s, want PRINTING during active CUPS execution", res.State)
		}
		if !res.PaperJam {
			t.Error("expected PaperJam = true")
		}
		if !strings.Contains(res.ErrorMessage, "paper jam") {
			t.Errorf("expected ErrorMessage to contain 'paper jam', got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_idle_snmp_cover_open_elevates_to_error", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			CoverOpen:       true,
			ErrorMessage:    "door open",
			EstimatedSheets: 200,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if !res.CoverOpen {
			t.Error("expected CoverOpen = true")
		}
		if !strings.Contains(res.ErrorMessage, "door open") {
			t.Errorf("expected ErrorMessage to contain 'door open', got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_idle_snmp_output_bin_full_elevates_to_error", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 180,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			OutputBinFull:   true,
			ErrorMessage:    "output bin full",
			EstimatedSheets: 180,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if !res.OutputBinFull {
			t.Error("expected OutputBinFull = true")
		}
	})

	t.Run("cups_online_snmp_unreachable_maintains_cups_online", func(t *testing.T) {
		// Requirement 10 & 12: Do not take down printer just because SNMP is disabled/unreachable
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "SNMP connection timeout",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if !res.Online {
			t.Error("expected Online = true based on CUPS queue operational state")
		}
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", res.State)
		}
		if res.TonerLevelPercent != 0 {
			t.Errorf("TonerLevelPercent = %d, want 0 (unavailable)", res.TonerLevelPercent)
		}
	})

	t.Run("cups_disabled_offline_marks_printer_unavailable", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			ErrorMessage:    "cups queue disabled",
			EstimatedSheets: 50,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			TonerLevelPercent: 75,
			EstimatedSheets:   50,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.Online {
			t.Error("expected Online = false when CUPS queue is disabled")
		}
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if res.TonerLevelPercent != 75 {
			t.Errorf("TonerLevelPercent = %d, want 75 (telemetry preserved)", res.TonerLevelPercent)
		}
	})

	t.Run("both_offline", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "printer not found in cups",
			EstimatedSheets: 0,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "snmp connection refused",
			EstimatedSheets: 0,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.Online {
			t.Error("expected Online = false")
		}
		if res.State != "OFFLINE" {
			t.Errorf("State = %s, want OFFLINE", res.State)
		}
		if !strings.Contains(res.ErrorMessage, "cups") || !strings.Contains(res.ErrorMessage, "snmp") {
			t.Errorf("expected combined error message, got %q", res.ErrorMessage)
		}
	})
}

func TestPollCombinedHealthSubsystems(t *testing.T) {
	t.Run("neither_configured", func(t *testing.T) {
		res := PollCombinedHealth(context.Background(), nil, "", SNMPConfig{}, 100)
		if res.Online {
			t.Error("expected Online = false when neither configured")
		}
		if res.State != "OFFLINE" {
			t.Errorf("State = %s, want OFFLINE", res.State)
		}
	})

	t.Run("cups_only_custom_runner", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("printer DCPT310 is idle. enabled since Thu Jan 01 00:00:00 2026"), nil
		}

		res := PollCombinedHealth(context.Background(), runner, "DCPT310", SNMPConfig{}, 150)
		if !res.Online {
			t.Error("expected Online = true")
		}
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", res.State)
		}
	})
}
