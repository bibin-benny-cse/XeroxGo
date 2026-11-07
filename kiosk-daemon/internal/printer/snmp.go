package printer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"xeroxgo/pkg/contracts"
)

const (
	defaultTarget    = "192.168.99.2"
	defaultPort      = 161
	defaultCommunity = "public"
	defaultTimeout   = 2 * time.Second
	defaultRetries   = 1

	// RFC 1213 / RFC 3418 MIB-II
	oidSysDescr = "1.3.6.1.2.1.1.1.0"

	// RFC 2790 Host Resources MIB
	oidHrPrinterStatus             = "1.3.6.1.2.1.25.3.5.1.1.1"
	oidHrPrinterDetectedErrorState = "1.3.6.1.2.1.25.3.5.1.2.1"

	// RFC 3805 / RFC 1759 Printer MIB
	oidPrtMarkerLifeCount      = "1.3.6.1.2.1.43.10.2.1.4.1.1"
	oidPrtMarkerSuppliesLevel1 = "1.3.6.1.2.1.43.11.1.1.9.1.1" // Supply 1 (toner) level
	oidPrtMarkerSuppliesMax1   = "1.3.6.1.2.1.43.11.1.1.8.1.1" // Supply 1 (toner) max capacity
	oidPrtMarkerSuppliesLevel2 = "1.3.6.1.2.1.43.11.1.1.9.1.2" // Supply 2 (drum/waste) level
	oidPrtMarkerSuppliesMax2   = "1.3.6.1.2.1.43.11.1.1.8.1.2" // Supply 2 (drum/waste) max capacity
)

// queriedOIDs contains the standard Host-Resources-MIB and Printer-MIB OIDs queried for health.
var queriedOIDs = []string{
	oidHrPrinterStatus,
	oidHrPrinterDetectedErrorState,
	oidPrtMarkerLifeCount,
	oidPrtMarkerSuppliesLevel1,
	oidPrtMarkerSuppliesMax1,
	oidPrtMarkerSuppliesLevel2,
	oidPrtMarkerSuppliesMax2,
}

// SNMPConfig encapsulates SNMP connection parameters.
type SNMPConfig struct {
	Target    string
	Port      int
	Community string
	Timeout   time.Duration
	Retries   int
}

// DefaultSNMPConfig returns standard configuration for printer SNMP monitoring.
func DefaultSNMPConfig() SNMPConfig {
	return SNMPConfig{
		Target:    defaultTarget,
		Port:      defaultPort,
		Community: defaultCommunity,
		Timeout:   defaultTimeout,
		Retries:   defaultRetries,
	}
}

// PrinterErrorFlags decodes RFC 2790 hrPrinterDetectedErrorState BITS.
type PrinterErrorFlags struct {
	LowPaper            bool // bit 0 (0x80)
	NoPaper             bool // bit 1 (0x40)
	LowToner            bool // bit 2 (0x20)
	NoToner             bool // bit 3 (0x10)
	DoorOpen            bool // bit 4 (0x08)
	Jammed              bool // bit 5 (0x04)
	Offline             bool // bit 6 (0x02)
	ServiceRequested    bool // bit 7 (0x01)
	InputTrayMissing    bool // bit 8 (0x80 in byte 1)
	OutputTrayMissing   bool // bit 9 (0x40 in byte 1)
	MarkerSupplyMissing bool // bit 10 (0x20 in byte 1)
	OutputNearFull      bool // bit 11 (0x10 in byte 1)
	OutputFull          bool // bit 12 (0x08 in byte 1)
	InputTrayEmpty      bool // bit 13 (0x04 in byte 1)
	OverduePreventMaint bool // bit 14 (0x02 in byte 1)
}

// DecodeErrorBitmask parses RFC 2790 hrPrinterDetectedErrorState octets.
func DecodeErrorBitmask(raw []byte) PrinterErrorFlags {
	var flags PrinterErrorFlags
	if len(raw) == 0 {
		return flags
	}

	b0 := raw[0]
	flags.LowPaper = b0&0x80 != 0
	flags.NoPaper = b0&0x40 != 0
	flags.LowToner = b0&0x20 != 0
	flags.NoToner = b0&0x10 != 0
	flags.DoorOpen = b0&0x08 != 0
	flags.Jammed = b0&0x04 != 0
	flags.Offline = b0&0x02 != 0
	flags.ServiceRequested = b0&0x01 != 0

	if len(raw) > 1 {
		b1 := raw[1]
		flags.InputTrayMissing = b1&0x80 != 0
		flags.OutputTrayMissing = b1&0x40 != 0
		flags.MarkerSupplyMissing = b1&0x20 != 0
		flags.OutputNearFull = b1&0x10 != 0
		flags.OutputFull = b1&0x08 != 0
		flags.InputTrayEmpty = b1&0x04 != 0
		flags.OverduePreventMaint = b1&0x02 != 0
	}
	return flags
}

type snmpSession interface {
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	Close() error
}

type realSnmpSession struct {
	client *gosnmp.GoSNMP
}

func (s *realSnmpSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	return s.client.Get(oids)
}

func (s *realSnmpSession) Close() error {
	if s.client != nil && s.client.Conn != nil {
		return s.client.Conn.Close()
	}
	return nil
}

type sessionFactory func(ctx context.Context) (snmpSession, error)

func realSessionFactoryWithConfig(cfg SNMPConfig) sessionFactory {
	return func(ctx context.Context) (snmpSession, error) {
		target := cfg.Target
		if target == "" {
			target = defaultTarget
		}
		port := uint16(cfg.Port)
		if port == 0 {
			port = defaultPort
		}
		community := cfg.Community
		if community == "" {
			community = defaultCommunity
		}
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		retries := cfg.Retries
		if retries < 0 {
			retries = 0
		}

		client := &gosnmp.GoSNMP{
			Target:    target,
			Port:      port,
			Community: community,
			Version:   gosnmp.Version2c,
			Timeout:   timeout,
			Retries:   retries,
			Context:   ctx,
		}

		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("failed to connect to printer at %s:%d: %w", target, port, err)
		}

		return &realSnmpSession{client: client}, nil
	}
}

func realSessionFactoryWithTarget(target string) sessionFactory {
	cfg := DefaultSNMPConfig()
	cfg.Target = target
	return realSessionFactoryWithConfig(cfg)
}

func realSessionFactory(ctx context.Context) (snmpSession, error) {
	return realSessionFactoryWithTarget(defaultTarget)(ctx)
}

func offlineHealth(estimatedSheets int, errMsg string) contracts.PrinterHealth {
	return contracts.PrinterHealth{
		Online:          false,
		State:           "OFFLINE",
		PaperTrayStatus: "UNKNOWN",
		EstimatedSheets: estimatedSheets,
		ErrorMessage:    errMsg,
		UpdatedAt:       time.Now().UTC(),
	}
}

func normalizeOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

func isValidPDU(pdu gosnmp.SnmpPDU) bool {
	if pdu.Type == gosnmp.NoSuchObject || pdu.Type == gosnmp.NoSuchInstance || pdu.Type == gosnmp.Null {
		return false
	}
	return true
}

func findPDU(packet *gosnmp.SnmpPacket, targetOID string, fallbackIndex int) (gosnmp.SnmpPDU, bool) {
	if packet == nil {
		return gosnmp.SnmpPDU{}, false
	}
	target := normalizeOID(targetOID)
	for _, pdu := range packet.Variables {
		if normalizeOID(pdu.Name) == target {
			return pdu, true
		}
	}
	if fallbackIndex >= 0 && fallbackIndex < len(packet.Variables) {
		pdu := packet.Variables[fallbackIndex]
		if pdu.Name == "" || normalizeOID(pdu.Name) == target {
			return pdu, true
		}
	}
	return gosnmp.SnmpPDU{}, false
}

func pduToInt64(pdu gosnmp.SnmpPDU) (int64, bool) {
	switch pdu.Type {
	case gosnmp.Integer, gosnmp.Counter32, gosnmp.Counter64, gosnmp.Gauge32, gosnmp.TimeTicks:
	default:
		return 0, false
	}
	switch v := pdu.Value.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	default:
		return 0, false
	}
}

// calcSupplyPercentage calculates remaining percentage following RFC 3805 semantics:
// Level: >= 0 (valid units), -1 (other), -2 (unknown), -3 (some remaining).
// MaxCapacity: > 0 (valid units), -1 (other), -2 (unknown).
// Only returns valid when maxCap > 0 and level >= 0.
func calcSupplyPercentage(pduLevel, pduMax gosnmp.SnmpPDU) (int, bool) {
	if !isValidPDU(pduLevel) || !isValidPDU(pduMax) {
		return 0, false
	}
	level, okL := pduToInt64(pduLevel)
	maxCap, okM := pduToInt64(pduMax)
	if !okL || !okM {
		return 0, false
	}
	if maxCap <= 0 || level < 0 {
		return 0, false
	}
	pct := int((level * 100) / maxCap)
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct, true
}

func pollHealthWithFactory(
	ctx context.Context,
	estimatedSheets int,
	factory sessionFactory,
) contracts.PrinterHealth {
	if ctx == nil {
		return offlineHealth(estimatedSheets, "context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return offlineHealth(estimatedSheets, fmt.Sprintf("context canceled: %v", err))
	}

	session, err := factory(ctx)
	if err != nil {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP connection failed: %v", err))
	}
	defer session.Close()

	packet, err := session.Get(queriedOIDs)
	if err != nil {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP GET request failed: %v", err))
	}
	if packet == nil {
		return offlineHealth(estimatedSheets, "SNMP response was nil")
	}

	// In SNMPv1 or strict agents, querying unsupported OIDs can return NoSuchName at packet level.
	// Fall back to essential Host-Resources-MIB OIDs if that occurs.
	if packet.Error == gosnmp.NoSuchName {
		essentialOIDs := []string{oidHrPrinterStatus, oidHrPrinterDetectedErrorState}
		fallbackPacket, fallbackErr := session.Get(essentialOIDs)
		if fallbackErr == nil && fallbackPacket != nil && fallbackPacket.Error == gosnmp.NoError {
			packet = fallbackPacket
		} else {
			return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP error status in response: %v (index %d)", packet.Error, packet.ErrorIndex))
		}
	} else if packet.Error != gosnmp.NoError {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP error status in response: %v (index %d)", packet.Error, packet.ErrorIndex))
	}

	// 1. hrPrinterStatus (1.3.6.1.2.1.25.3.5.1.1.1)
	// RFC 2790: 1=other, 2=unknown, 3=idle, 4=printing, 5=warmup
	statusState := "UNKNOWN"
	statusFound := false
	var statusVal int

	pduStatus, okStatus := findPDU(packet, oidHrPrinterStatus, 0)
	if okStatus && isValidPDU(pduStatus) {
		if v, okVal := pduToInt64(pduStatus); okVal {
			statusVal = int(v)
			statusFound = true
			switch statusVal {
			case 1, 2:
				statusState = "UNKNOWN"
			case 3:
				statusState = "IDLE"
			case 4:
				statusState = "PRINTING"
			case 5:
				statusState = "WARMING_UP"
			case 6:
				statusState = "ERROR"
			case 7:
				statusState = "OFFLINE"
			default:
				statusState = "UNKNOWN"
			}
		}
	}

	// 2. hrPrinterDetectedErrorState (1.3.6.1.2.1.25.3.5.1.2.1)
	var flags PrinterErrorFlags
	errorFlagsFound := false
	pduErrorState, okErr := findPDU(packet, oidHrPrinterDetectedErrorState, 1)
	if okErr && isValidPDU(pduErrorState) {
		var rawBytes []byte
		switch v := pduErrorState.Value.(type) {
		case []byte:
			rawBytes = v
		case string:
			rawBytes = []byte(v)
		}
		if len(rawBytes) > 0 {
			flags = DecodeErrorBitmask(rawBytes)
			errorFlagsFound = true
		}
	}

	// 3. prtMarkerLifeCount (1.3.6.1.2.1.43.10.2.1.4.1.1)
	markerFound := false
	pduMarkerLife, okMarker := findPDU(packet, oidPrtMarkerLifeCount, 2)
	if okMarker && isValidPDU(pduMarkerLife) {
		if _, okVal := pduToInt64(pduMarkerLife); okVal {
			markerFound = true
		}
	}

	// 4. prtMarkerSuppliesLevel & MaxCapacity (Supply 1 - Toner)
	tonerPercent := 0
	pduTonerLevel, okTL := findPDU(packet, oidPrtMarkerSuppliesLevel1, 3)
	pduTonerMax, okTM := findPDU(packet, oidPrtMarkerSuppliesMax1, 4)
	if okTL && okTM {
		if pct, valid := calcSupplyPercentage(pduTonerLevel, pduTonerMax); valid {
			tonerPercent = pct
		}
	}

	// 5. prtMarkerSuppliesLevel & MaxCapacity (Supply 2 - Drum Unit)
	drumPercent := 0
	pduDrumLevel, okDL := findPDU(packet, oidPrtMarkerSuppliesLevel2, 5)
	pduDrumMax, okDM := findPDU(packet, oidPrtMarkerSuppliesMax2, 6)
	if okDL && okDM {
		if pct, valid := calcSupplyPercentage(pduDrumLevel, pduDrumMax); valid {
			drumPercent = pct
		}
	}

	// Distinguish actual communication failure from missing individual metrics.
	// If the SNMP agent responded but returned zero recognizable metrics, report UNKNOWN.
	if !statusFound && !errorFlagsFound && !markerFound && tonerPercent == 0 && drumPercent == 0 {
		return contracts.PrinterHealth{
			Online:            true,
			State:             "UNKNOWN",
			PaperTrayStatus:   "UNKNOWN",
			EstimatedSheets:   estimatedSheets,
			TonerLevelPercent: 0,
			DrumLifePercent:   0,
			CoverOpen:         false,
			PaperJam:          false,
			OutputBinFull:     false,
			ErrorMessage:      "SNMP responded but returned no supported printer metrics",
			UpdatedAt:         time.Now().UTC(),
		}
	}

	// PaperTrayStatus resolution
	paperTrayStatus := "OK"
	if flags.NoPaper || flags.InputTrayEmpty || flags.InputTrayMissing {
		paperTrayStatus = "EMPTY"
	} else if flags.LowPaper {
		paperTrayStatus = "LOW"
	}

	// Online and State mapping
	isOffline := (statusState == "OFFLINE") || flags.Offline
	var online bool
	var finalState string

	if isOffline {
		online = false
		finalState = "OFFLINE"
	} else {
		online = true
		if flags.DoorOpen || flags.Jammed || flags.OutputFull || flags.NoPaper || flags.NoToner || flags.InputTrayEmpty || flags.ServiceRequested {
			finalState = "ERROR"
		} else {
			finalState = statusState
		}
	}

	// Formulation of descriptive ErrorMessage
	var issues []string
	if isOffline {
		issues = append(issues, "printer offline")
	}
	if flags.DoorOpen {
		issues = append(issues, "door open")
	}
	if flags.Jammed {
		issues = append(issues, "paper jam")
	}
	if flags.OutputFull {
		issues = append(issues, "output bin full")
	} else if flags.OutputNearFull {
		issues = append(issues, "output bin near full")
	}
	if flags.NoPaper {
		issues = append(issues, "no paper")
	} else if flags.InputTrayEmpty {
		issues = append(issues, "input tray empty")
	} else if flags.InputTrayMissing {
		issues = append(issues, "input tray missing")
	} else if flags.LowPaper {
		issues = append(issues, "low paper")
	}
	if flags.NoToner {
		issues = append(issues, "no toner")
	} else if flags.LowToner {
		issues = append(issues, "low toner")
	}
	if flags.MarkerSupplyMissing {
		issues = append(issues, "marker supply missing")
	}
	if flags.ServiceRequested {
		issues = append(issues, "service requested")
	}
	if flags.OverduePreventMaint {
		issues = append(issues, "maintenance overdue")
	}

	if finalState == "ERROR" && len(issues) == 0 {
		if statusVal == 6 {
			issues = append(issues, "printer error")
		} else {
			issues = append(issues, "unrecognized printer error")
		}
	} else if finalState == "UNKNOWN" && len(issues) == 0 && statusFound {
		issues = append(issues, fmt.Sprintf("unrecognized printer status: %d", statusVal))
	}

	errorMsg := strings.Join(issues, ", ")

	return contracts.PrinterHealth{
		Online:            online,
		State:             finalState,
		PaperTrayStatus:   paperTrayStatus,
		EstimatedSheets:   estimatedSheets,
		TonerLevelPercent: tonerPercent,
		DrumLifePercent:   drumPercent,
		CoverOpen:         flags.DoorOpen,
		PaperJam:          flags.Jammed,
		OutputBinFull:     flags.OutputFull,
		ErrorMessage:      errorMsg,
		UpdatedAt:         time.Now().UTC(),
	}
}

// PollHealthConfig polls printer hardware health over SNMP with the provided configuration.
func PollHealthConfig(
	ctx context.Context,
	cfg SNMPConfig,
	estimatedSheets int,
) contracts.PrinterHealth {
	return pollHealthWithFactory(ctx, estimatedSheets, realSessionFactoryWithConfig(cfg))
}

// PollHealthTarget polls printer hardware health over SNMP targeting a specific host/IP.
func PollHealthTarget(
	ctx context.Context,
	target string,
	estimatedSheets int,
) contracts.PrinterHealth {
	cfg := DefaultSNMPConfig()
	cfg.Target = target
	return PollHealthConfig(ctx, cfg, estimatedSheets)
}

// PollHealth polls printer hardware health over SNMP using the default address.
func PollHealth(
	ctx context.Context,
	estimatedSheets int,
) contracts.PrinterHealth {
	return PollHealthTarget(ctx, defaultTarget, estimatedSheets)
}
