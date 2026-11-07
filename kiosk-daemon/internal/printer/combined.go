package printer

import (
	"context"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// CombineHealth merges CUPS queue availability with SNMP device health telemetry.
//
// Authoritative separation of responsibilities:
// 1. CUPS is authoritative for queue status and active print job execution.
//    If CUPS is actively PRINTING, the combined state remains PRINTING.
// 2. SNMP is authoritative for physical hardware sensors:
//    - TonerLevelPercent
//    - DrumLifePercent
//    - CoverOpen (door open)
//    - PaperJam (jammed)
//    - OutputBinFull (output tray full)
//    - PaperTrayStatus (empty/low paper)
// 3. If CUPS is online and SNMP is unreachable (network timeout / SNMP disabled),
//    the printer remains online based on CUPS availability without disrupting the kiosk.
// 4. If physical hardware faults (jam, door open, out of paper, bin full) occur,
//    they are elevated to ErrorMessage and (when idle) transition the state to ERROR.
func CombineHealth(cupsHealth contracts.PrinterHealth, snmpHealth contracts.PrinterHealth) contracts.PrinterHealth {
	now := time.Now().UTC()

	// Case 1: Both subsystems report offline
	if !cupsHealth.Online && !snmpHealth.Online {
		combinedErr := cupsHealth.ErrorMessage
		if snmpHealth.ErrorMessage != "" && !strings.Contains(combinedErr, snmpHealth.ErrorMessage) {
			if combinedErr != "" {
				combinedErr += "; " + snmpHealth.ErrorMessage
			} else {
				combinedErr = snmpHealth.ErrorMessage
			}
		}
		return contracts.PrinterHealth{
			Online:            false,
			State:             "OFFLINE",
			PaperTrayStatus:   "UNKNOWN",
			EstimatedSheets:   cupsHealth.EstimatedSheets,
			TonerLevelPercent: snmpHealth.TonerLevelPercent,
			DrumLifePercent:   snmpHealth.DrumLifePercent,
			CoverOpen:         snmpHealth.CoverOpen,
			PaperJam:          snmpHealth.PaperJam,
			OutputBinFull:     snmpHealth.OutputBinFull,
			ErrorMessage:      combinedErr,
			UpdatedAt:         now,
		}
	}

	// Case 2: CUPS queue is offline / disabled. Spooler cannot send jobs.
	if !cupsHealth.Online {
		combinedErr := cupsHealth.ErrorMessage
		if snmpHealth.ErrorMessage != "" && !strings.Contains(combinedErr, snmpHealth.ErrorMessage) {
			if combinedErr != "" {
				combinedErr += "; " + snmpHealth.ErrorMessage
			} else {
				combinedErr = snmpHealth.ErrorMessage
			}
		}
		return contracts.PrinterHealth{
			Online:            false,
			State:             cupsHealth.State,
			PaperTrayStatus:   snmpHealth.PaperTrayStatus,
			EstimatedSheets:   cupsHealth.EstimatedSheets,
			TonerLevelPercent: snmpHealth.TonerLevelPercent,
			DrumLifePercent:   snmpHealth.DrumLifePercent,
			CoverOpen:         snmpHealth.CoverOpen,
			PaperJam:          snmpHealth.PaperJam,
			OutputBinFull:     snmpHealth.OutputBinFull,
			ErrorMessage:      combinedErr,
			UpdatedAt:         now,
		}
	}

	// Case 3: CUPS is online, but SNMP is unreachable. Maintain printer readiness from CUPS.
	if !snmpHealth.Online {
		return contracts.PrinterHealth{
			Online:            true,
			State:             cupsHealth.State,
			PaperTrayStatus:   cupsHealth.PaperTrayStatus,
			EstimatedSheets:   cupsHealth.EstimatedSheets,
			TonerLevelPercent: 0,
			DrumLifePercent:   0,
			CoverOpen:         false,
			PaperJam:          false,
			OutputBinFull:     false,
			ErrorMessage:      cupsHealth.ErrorMessage,
			UpdatedAt:         now,
		}
	}

	// Case 4: Both CUPS and SNMP are online.
	tonerPct := snmpHealth.TonerLevelPercent
	drumPct := snmpHealth.DrumLifePercent
	coverOpen := snmpHealth.CoverOpen
	paperJam := snmpHealth.PaperJam
	outputBinFull := snmpHealth.OutputBinFull

	// Physical paper status from SNMP takes precedence over CUPS heuristics
	paperTray := "OK"
	if snmpHealth.PaperTrayStatus != "OK" && snmpHealth.PaperTrayStatus != "" && snmpHealth.PaperTrayStatus != "UNKNOWN" {
		paperTray = snmpHealth.PaperTrayStatus
	} else if cupsHealth.PaperTrayStatus != "OK" && cupsHealth.PaperTrayStatus != "UNKNOWN" {
		paperTray = cupsHealth.PaperTrayStatus
	}

	// State determination:
	// Rule 13: Never overwrite a confirmed CUPS printing state with an unsupported SNMP status.
	finalState := cupsHealth.State
	if cupsHealth.State == "PRINTING" {
		finalState = "PRINTING"
	} else {
		// When idle or ready, elevate physical hardware faults to ERROR
		if coverOpen || paperJam || outputBinFull || paperTray == "EMPTY" || snmpHealth.State == "ERROR" {
			finalState = "ERROR"
		} else if snmpHealth.State == "WARMING_UP" {
			finalState = "WARMING_UP"
		} else if cupsHealth.State == "READY" || cupsHealth.State == "IDLE" {
			finalState = "IDLE"
		}
	}

	// Consolidate non-duplicate error messages
	var combinedIssues []string
	if cupsHealth.ErrorMessage != "" {
		combinedIssues = append(combinedIssues, cupsHealth.ErrorMessage)
	}
	if snmpHealth.ErrorMessage != "" {
		for _, issue := range strings.Split(snmpHealth.ErrorMessage, ", ") {
			issue = strings.TrimSpace(issue)
			if issue != "" && !strings.Contains(strings.ToLower(cupsHealth.ErrorMessage), strings.ToLower(issue)) {
				combinedIssues = append(combinedIssues, issue)
			}
		}
	}

	return contracts.PrinterHealth{
		Online:            true,
		State:             finalState,
		PaperTrayStatus:   paperTray,
		EstimatedSheets:   cupsHealth.EstimatedSheets,
		TonerLevelPercent: tonerPct,
		DrumLifePercent:   drumPct,
		CoverOpen:         coverOpen,
		PaperJam:          paperJam,
		OutputBinFull:     outputBinFull,
		ErrorMessage:      strings.Join(combinedIssues, ", "),
		UpdatedAt:         now,
	}
}

// PollCombinedHealth polls both CUPS queue status and SNMP hardware telemetry and combines them.
func PollCombinedHealth(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	snmpCfg SNMPConfig,
	estimatedSheets int,
) contracts.PrinterHealth {
	hasCUPS := strings.TrimSpace(queue) != ""
	hasSNMP := strings.TrimSpace(snmpCfg.Target) != ""

	if hasCUPS && !hasSNMP {
		if runner != nil {
			return PollCUPSHealthCustom(ctx, runner, queue, estimatedSheets)
		}
		return PollCUPSHealth(ctx, queue, estimatedSheets)
	}
	if !hasCUPS && hasSNMP {
		return PollHealthConfig(ctx, snmpCfg, estimatedSheets)
	}
	if !hasCUPS && !hasSNMP {
		return offlineHealth(estimatedSheets, "neither CUPS queue nor printer address configured")
	}

	// Both CUPS and SNMP configured
	var cupsHealth contracts.PrinterHealth
	if runner != nil {
		cupsHealth = PollCUPSHealthCustom(ctx, runner, queue, estimatedSheets)
	} else {
		cupsHealth = PollCUPSHealth(ctx, queue, estimatedSheets)
	}

	snmpHealth := PollHealthConfig(ctx, snmpCfg, estimatedSheets)

	return CombineHealth(cupsHealth, snmpHealth)
}
