package printer

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"
)

type fakeSnmpSession struct {
	getFunc   func(oids []string) (*gosnmp.SnmpPacket, error)
	closeFunc func() error
	closed    bool
	gotOIDs   []string
}

func (f *fakeSnmpSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	f.gotOIDs = append([]string(nil), oids...)
	if f.getFunc != nil {
		return f.getFunc(oids)
	}
	return nil, errors.New("getFunc not implemented")
}

func (f *fakeSnmpSession) Close() error {
	f.closed = true
	if f.closeFunc != nil {
		return f.closeFunc()
	}
	return nil
}

func createValidPacket(status int, errorBytes []byte, markerCount int64, tonerLevel, tonerMax int, drumLevel, drumMax int) *gosnmp.SnmpPacket {
	vars := []gosnmp.SnmpPDU{
		{
			Name:  oidHrPrinterStatus,
			Type:  gosnmp.Integer,
			Value: status,
		},
		{
			Name:  oidHrPrinterDetectedErrorState,
			Type:  gosnmp.OctetString,
			Value: errorBytes,
		},
		{
			Name:  oidPrtMarkerLifeCount,
			Type:  gosnmp.Counter32,
			Value: uint32(markerCount),
		},
		{
			Name:  oidPrtMarkerSuppliesLevel1,
			Type:  gosnmp.Integer,
			Value: tonerLevel,
		},
		{
			Name:  oidPrtMarkerSuppliesMax1,
			Type:  gosnmp.Integer,
			Value: tonerMax,
		},
		{
			Name:  oidPrtMarkerSuppliesLevel2,
			Type:  gosnmp.Integer,
			Value: drumLevel,
		},
		{
			Name:  oidPrtMarkerSuppliesMax2,
			Type:  gosnmp.Integer,
			Value: drumMax,
		},
	}

	return &gosnmp.SnmpPacket{
		Error:     gosnmp.NoError,
		Variables: vars,
	}
}

func TestDecodeErrorBitmask(t *testing.T) {
	t.Run("empty_input", func(t *testing.T) {
		flags := DecodeErrorBitmask([]byte{})
		expected := PrinterErrorFlags{}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("zero_bytes", func(t *testing.T) {
		flags := DecodeErrorBitmask([]byte{0x00, 0x00})
		expected := PrinterErrorFlags{}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("byte_0_individual_bits", func(t *testing.T) {
		tests := []struct {
			name     string
			input    byte
			expected PrinterErrorFlags
		}{
			{"LowPaper", 0x80, PrinterErrorFlags{LowPaper: true}},
			{"NoPaper", 0x40, PrinterErrorFlags{NoPaper: true}},
			{"LowToner", 0x20, PrinterErrorFlags{LowToner: true}},
			{"NoToner", 0x10, PrinterErrorFlags{NoToner: true}},
			{"DoorOpen", 0x08, PrinterErrorFlags{DoorOpen: true}},
			{"Jammed", 0x04, PrinterErrorFlags{Jammed: true}},
			{"Offline", 0x02, PrinterErrorFlags{Offline: true}},
			{"ServiceRequested", 0x01, PrinterErrorFlags{ServiceRequested: true}},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				flags := DecodeErrorBitmask([]byte{tc.input})
				if flags != tc.expected {
					t.Errorf("input 0x%02X: got %+v, want %+v", tc.input, flags, tc.expected)
				}
			})
		}
	})

	t.Run("byte_1_individual_bits", func(t *testing.T) {
		tests := []struct {
			name     string
			input    byte
			expected PrinterErrorFlags
		}{
			{"InputTrayMissing", 0x80, PrinterErrorFlags{InputTrayMissing: true}},
			{"OutputTrayMissing", 0x40, PrinterErrorFlags{OutputTrayMissing: true}},
			{"MarkerSupplyMissing", 0x20, PrinterErrorFlags{MarkerSupplyMissing: true}},
			{"OutputNearFull", 0x10, PrinterErrorFlags{OutputNearFull: true}},
			{"OutputFull", 0x08, PrinterErrorFlags{OutputFull: true}},
			{"InputTrayEmpty", 0x04, PrinterErrorFlags{InputTrayEmpty: true}},
			{"OverduePreventMaint", 0x02, PrinterErrorFlags{OverduePreventMaint: true}},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				flags := DecodeErrorBitmask([]byte{0x00, tc.input})
				if flags != tc.expected {
					t.Errorf("byte 1 0x%02X: got %+v, want %+v", tc.input, flags, tc.expected)
				}
			})
		}
	})

	t.Run("multiple_bytes_combined", func(t *testing.T) {
		// byte 0: DoorOpen (0x08) | Jammed (0x04)
		// byte 1: OutputFull (0x08) | InputTrayEmpty (0x04)
		flags := DecodeErrorBitmask([]byte{0x08 | 0x04, 0x08 | 0x04})
		expected := PrinterErrorFlags{
			DoorOpen:       true,
			Jammed:         true,
			OutputFull:     true,
			InputTrayEmpty: true,
		}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("input_slice_unchanged", func(t *testing.T) {
		orig := []byte{0x80, 0x20, 0x04}
		copyBuf := make([]byte, len(orig))
		copy(copyBuf, orig)

		_ = DecodeErrorBitmask(orig)

		if !bytes.Equal(orig, copyBuf) {
			t.Errorf("input slice modified: got %v, want %v", orig, copyBuf)
		}
	})
}

func TestPollHealthStatusMapping(t *testing.T) {
	tests := []struct {
		name      string
		statusVal int
		wantState string
		wantErr   bool
	}{
		{"IDLE", 3, "IDLE", false},
		{"PRINTING", 4, "PRINTING", false},
		{"WARMING_UP", 5, "WARMING_UP", false},
		{"ERROR", 6, "ERROR", true},
		{"OFFLINE", 7, "OFFLINE", true},
		{"UNKNOWN_1", 1, "UNKNOWN", true},
		{"UNKNOWN_2", 2, "UNKNOWN", true},
		{"UNKNOWN_99", 99, "UNKNOWN", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recordedOIDs []string
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					recordedOIDs = append([]string(nil), oids...)
					return createValidPacket(tc.statusVal, []byte{0x00}, 1000, 80, 100, 90, 100), nil
				},
			}

			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			ctx := context.Background()
			health := pollHealthWithFactory(ctx, 200, factory)

			if health.State != tc.wantState {
				t.Errorf("State = %s, want %s", health.State, tc.wantState)
			}
			if tc.wantState == "OFFLINE" && health.Online {
				t.Errorf("printer reported OFFLINE must have Online=false")
			}
			if tc.wantState != "OFFLINE" && !health.Online {
				t.Errorf("printer with state %s should have Online=true", tc.wantState)
			}
			if tc.wantErr && health.ErrorMessage == "" {
				t.Errorf("expected non-empty ErrorMessage for state %s", tc.wantState)
			}
			if !tc.wantErr && health.ErrorMessage != "" {
				t.Errorf("expected empty ErrorMessage for healthy state, got %q", health.ErrorMessage)
			}

			// Verify queried OIDs
			if !reflect.DeepEqual(recordedOIDs, queriedOIDs) {
				t.Errorf("queried OIDs = %v, want %v", recordedOIDs, queriedOIDs)
			}
		})
	}
}

func TestPollHealthErrorBitmask(t *testing.T) {
	tests := []struct {
		name         string
		statusVal    int
		errorBytes   []byte
		wantOnline   bool
		wantState    string
		wantTray     string
		wantCover    bool
		wantJam      bool
		wantBinFull  bool
		wantMsgWords []string
	}{
		{
			name:         "low_paper",
			statusVal:    3,
			errorBytes:   []byte{0x80},
			wantOnline:   true,
			wantState:    "IDLE",
			wantTray:     "LOW",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"low paper"},
		},
		{
			name:         "no_paper",
			statusVal:    3,
			errorBytes:   []byte{0x40},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "EMPTY",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"no paper"},
		},
		{
			name:         "low_toner",
			statusVal:    3,
			errorBytes:   []byte{0x20},
			wantOnline:   true,
			wantState:    "IDLE",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"low toner"},
		},
		{
			name:         "no_toner",
			statusVal:    3,
			errorBytes:   []byte{0x10},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"no toner"},
		},
		{
			name:         "door_open",
			statusVal:    3,
			errorBytes:   []byte{0x08},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    true,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"door open"},
		},
		{
			name:         "paper_jam",
			statusVal:    3,
			errorBytes:   []byte{0x04},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      true,
			wantBinFull:  false,
			wantMsgWords: []string{"paper jam"},
		},
		{
			name:         "offline_bit",
			statusVal:    3,
			errorBytes:   []byte{0x02},
			wantOnline:   false,
			wantState:    "OFFLINE",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"offline"},
		},
		{
			name:         "service_requested",
			statusVal:    3,
			errorBytes:   []byte{0x01},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"service requested"},
		},
		{
			name:         "output_bin_full",
			statusVal:    3,
			errorBytes:   []byte{0x00, 0x08},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  true,
			wantMsgWords: []string{"output bin full"},
		},
		{
			name:         "input_tray_empty_byte1",
			statusVal:    3,
			errorBytes:   []byte{0x00, 0x04},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "EMPTY",
			wantCover:    false,
			wantJam:      false,
			wantBinFull:  false,
			wantMsgWords: []string{"input tray empty"},
		},
		{
			name:         "door_open_and_jam",
			statusVal:    3,
			errorBytes:   []byte{0x08 | 0x04},
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    true,
			wantJam:      true,
			wantBinFull:  false,
			wantMsgWords: []string{"door open", "paper jam"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					return createValidPacket(tc.statusVal, tc.errorBytes, 2500, 75, 100, 80, 100), nil
				},
			}
			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			health := pollHealthWithFactory(context.Background(), 250, factory)

			if health.Online != tc.wantOnline {
				t.Errorf("Online = %v, want %v", health.Online, tc.wantOnline)
			}
			if health.State != tc.wantState {
				t.Errorf("State = %s, want %s", health.State, tc.wantState)
			}
			if health.PaperTrayStatus != tc.wantTray {
				t.Errorf("PaperTrayStatus = %s, want %s", health.PaperTrayStatus, tc.wantTray)
			}
			if health.CoverOpen != tc.wantCover {
				t.Errorf("CoverOpen = %v, want %v", health.CoverOpen, tc.wantCover)
			}
			if health.PaperJam != tc.wantJam {
				t.Errorf("PaperJam = %v, want %v", health.PaperJam, tc.wantJam)
			}
			if health.OutputBinFull != tc.wantBinFull {
				t.Errorf("OutputBinFull = %v, want %v", health.OutputBinFull, tc.wantBinFull)
			}

			for _, word := range tc.wantMsgWords {
				if !strings.Contains(strings.ToLower(health.ErrorMessage), word) {
					t.Errorf("ErrorMessage %q does not contain expected %q", health.ErrorMessage, word)
				}
			}
		})
	}
}

func TestSupplyLevelCalculations(t *testing.T) {
	tests := []struct {
		name        string
		tonerLevel  int
		tonerMax    int
		drumLevel   int
		drumMax     int
		wantToner   int
		wantDrum    int
	}{
		{
			name:       "normal_percentages",
			tonerLevel: 75,
			tonerMax:   100,
			drumLevel:  8500,
			drumMax:    10000,
			wantToner:  75,
			wantDrum:   85,
		},
		{
			name:       "empty_supplies",
			tonerLevel: 0,
			tonerMax:   100,
			drumLevel:  0,
			drumMax:    10000,
			wantToner:  0,
			wantDrum:   0,
		},
		{
			name:       "overflow_clamped_to_100",
			tonerLevel: 150,
			tonerMax:   100,
			drumLevel:  12000,
			drumMax:    10000,
			wantToner:  100,
			wantDrum:   100,
		},
		{
			name:       "rfc3805_negative_levels_unknown",
			tonerLevel: -3, // some remaining, not quantifiable
			tonerMax:   100,
			drumLevel:  -2, // unknown
			drumMax:    10000,
			wantToner:  0,
			wantDrum:   0,
		},
		{
			name:       "zero_or_negative_max_capacity",
			tonerLevel: 50,
			tonerMax:   0,
			drumLevel:  50,
			drumMax:    -1,
			wantToner:  0,
			wantDrum:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					return createValidPacket(3, []byte{0x00}, 1000, tc.tonerLevel, tc.tonerMax, tc.drumLevel, tc.drumMax), nil
				},
			}
			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			health := pollHealthWithFactory(context.Background(), 200, factory)

			if health.TonerLevelPercent != tc.wantToner {
				t.Errorf("TonerLevelPercent = %d, want %d", health.TonerLevelPercent, tc.wantToner)
			}
			if health.DrumLifePercent != tc.wantDrum {
				t.Errorf("DrumLifePercent = %d, want %d", health.DrumLifePercent, tc.wantDrum)
			}
		})
	}
}

func TestPartialSNMPSupportAndMissingOIDs(t *testing.T) {
	t.Run("missing_supplies_and_marker_count_still_reports_online", func(t *testing.T) {
		// HP LaserJet Tank 1020w scenario: returns status and error state, but does not provide supplies or marker count
		pkt := &gosnmp.SnmpPacket{
			Error: gosnmp.NoError,
			Variables: []gosnmp.SnmpPDU{
				{
					Name:  oidHrPrinterStatus,
					Type:  gosnmp.Integer,
					Value: 3, // IDLE
				},
				{
					Name:  oidHrPrinterDetectedErrorState,
					Type:  gosnmp.OctetString,
					Value: []byte{0x00},
				},
				{
					Name: oidPrtMarkerLifeCount,
					Type: gosnmp.NoSuchObject,
				},
				{
					Name: oidPrtMarkerSuppliesLevel1,
					Type: gosnmp.NoSuchObject,
				},
				{
					Name: oidPrtMarkerSuppliesMax1,
					Type: gosnmp.NoSuchObject,
				},
			},
		}

		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return pkt, nil
			},
		}
		factory := func(ctx context.Context) (snmpSession, error) {
			return session, nil
		}

		health := pollHealthWithFactory(context.Background(), 100, factory)

		if !health.Online {
			t.Errorf("expected Online = true for partially supported SNMP device, got false: %s", health.ErrorMessage)
		}
		if health.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", health.State)
		}
		if health.TonerLevelPercent != 0 {
			t.Errorf("TonerLevelPercent = %d, want 0", health.TonerLevelPercent)
		}
	})

	t.Run("missing_status_oid_with_other_metrics_reports_unknown_not_offline", func(t *testing.T) {
		pkt := &gosnmp.SnmpPacket{
			Error: gosnmp.NoError,
			Variables: []gosnmp.SnmpPDU{
				{
					Name:  oidHrPrinterDetectedErrorState,
					Type:  gosnmp.OctetString,
					Value: []byte{0x00},
				},
				{
					Name:  oidPrtMarkerLifeCount,
					Type:  gosnmp.Counter32,
					Value: uint32(500),
				},
			},
		}

		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return pkt, nil
			},
		}
		factory := func(ctx context.Context) (snmpSession, error) {
			return session, nil
		}

		health := pollHealthWithFactory(context.Background(), 100, factory)

		if !health.Online {
			t.Errorf("expected Online = true when device answers SNMP, got false: %s", health.ErrorMessage)
		}
		if health.State != "UNKNOWN" {
			t.Errorf("State = %s, want UNKNOWN", health.State)
		}
	})

	t.Run("all_oids_nosuchobject_reports_unknown_device", func(t *testing.T) {
		pkt := &gosnmp.SnmpPacket{
			Error: gosnmp.NoError,
			Variables: []gosnmp.SnmpPDU{
				{Name: oidHrPrinterStatus, Type: gosnmp.NoSuchObject},
				{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.NoSuchObject},
			},
		}

		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return pkt, nil
			},
		}
		factory := func(ctx context.Context) (snmpSession, error) {
			return session, nil
		}

		health := pollHealthWithFactory(context.Background(), 100, factory)

		if !health.Online {
			t.Errorf("expected Online = true since agent answered SNMP, got false: %s", health.ErrorMessage)
		}
		if health.State != "UNKNOWN" {
			t.Errorf("State = %s, want UNKNOWN", health.State)
		}
		if !strings.Contains(health.ErrorMessage, "no supported printer metrics") {
			t.Errorf("expected ErrorMessage to mention no supported metrics, got %q", health.ErrorMessage)
		}
	})

	t.Run("snmpv1_nosuchname_fallback_recovers_essential_status", func(t *testing.T) {
		firstCall := true
		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				if firstCall {
					firstCall = false
					// Initial query fails because agent doesn't recognize supply OIDs
					return &gosnmp.SnmpPacket{
						Error:      gosnmp.NoSuchName,
						ErrorIndex: 4,
					}, nil
				}
				// Fallback with essential OIDs succeeds
				return &gosnmp.SnmpPacket{
					Error: gosnmp.NoError,
					Variables: []gosnmp.SnmpPDU{
						{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: 3},
						{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
					},
				}, nil
			},
		}
		factory := func(ctx context.Context) (snmpSession, error) {
			return session, nil
		}

		health := pollHealthWithFactory(context.Background(), 150, factory)

		if !health.Online {
			t.Errorf("expected fallback to recover Online=true, got false: %s", health.ErrorMessage)
		}
		if health.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", health.State)
		}
	})
}

func TestPollHealthUsesEstimatedSheets(t *testing.T) {
	session := &fakeSnmpSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return createValidPacket(3, []byte{0x00}, 100, 80, 100, 90, 100), nil
		},
	}
	factory := func(ctx context.Context) (snmpSession, error) {
		return session, nil
	}

	health := pollHealthWithFactory(context.Background(), 173, factory)
	if health.EstimatedSheets != 173 {
		t.Errorf("EstimatedSheets = %d, want 173", health.EstimatedSheets)
	}
}

func TestPollHealthRequestFailure(t *testing.T) {
	session := &fakeSnmpSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return nil, errors.New("network timeout")
		},
	}
	factory := func(ctx context.Context) (snmpSession, error) {
		return session, nil
	}

	health := pollHealthWithFactory(context.Background(), 150, factory)

	if health.Online {
		t.Error("expected Online = false on request failure")
	}
	if health.State != "OFFLINE" {
		t.Errorf("State = %s, want OFFLINE", health.State)
	}
	if health.EstimatedSheets != 150 {
		t.Errorf("EstimatedSheets = %d, want 150", health.EstimatedSheets)
	}
	if !strings.Contains(health.ErrorMessage, "network timeout") {
		t.Errorf("expected ErrorMessage to contain 'network timeout', got %q", health.ErrorMessage)
	}
	if !session.closed {
		t.Error("expected fake session to be closed")
	}
}

func TestPollHealthMalformedResponses(t *testing.T) {
	tests := []struct {
		name      string
		pkt       *gosnmp.SnmpPacket
		errSubstr string
	}{
		{
			name:      "nil_packet",
			pkt:       nil,
			errSubstr: "nil",
		},
		{
			name: "snmp_bad_value_packet_error",
			pkt: &gosnmp.SnmpPacket{
				Error:      gosnmp.BadValue,
				ErrorIndex: 1,
			},
			errSubstr: "SNMP error status",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					return tc.pkt, nil
				},
			}
			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			health := pollHealthWithFactory(context.Background(), 250, factory)

			if health.Online {
				t.Errorf("expected Online = false for malformed response case %s", tc.name)
			}
			if health.State != "OFFLINE" {
				t.Errorf("expected State = OFFLINE for case %s, got %s", tc.name, health.State)
			}
			if !strings.Contains(strings.ToLower(health.ErrorMessage), strings.ToLower(tc.errSubstr)) {
				t.Errorf("ErrorMessage %q does not contain %q", health.ErrorMessage, tc.errSubstr)
			}
		})
	}
}

func TestPollHealthCanceledContext(t *testing.T) {
	sessionCreated := false
	factory := func(ctx context.Context) (snmpSession, error) {
		sessionCreated = true
		return &fakeSnmpSession{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	health := pollHealthWithFactory(ctx, 180, factory)

	if sessionCreated {
		t.Error("expected session not to be created when context is already canceled")
	}
	if health.Online {
		t.Error("expected Online = false for canceled context")
	}
	if health.State != "OFFLINE" {
		t.Errorf("State = %s, want OFFLINE", health.State)
	}
	if !strings.Contains(health.ErrorMessage, "canceled") {
		t.Errorf("expected ErrorMessage to contain 'canceled', got %q", health.ErrorMessage)
	}
}

func TestPollHealthConnectFailure(t *testing.T) {
	factory := func(ctx context.Context) (snmpSession, error) {
		return nil, errors.New("connection refused")
	}

	health := pollHealthWithFactory(context.Background(), 210, factory)

	if health.Online {
		t.Error("expected Online = false on connection failure")
	}
	if health.State != "OFFLINE" {
		t.Errorf("State = %s, want OFFLINE", health.State)
	}
	if !strings.Contains(health.ErrorMessage, "connection refused") {
		t.Errorf("expected ErrorMessage to contain 'connection refused', got %q", health.ErrorMessage)
	}
}

func TestPollHealthClosesSession(t *testing.T) {
	session := &fakeSnmpSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return nil, errors.New("corrupt response")
		},
	}
	factory := func(ctx context.Context) (snmpSession, error) {
		return session, nil
	}

	_ = pollHealthWithFactory(context.Background(), 250, factory)

	if !session.closed {
		t.Error("expected session to be closed even on malformed/error response")
	}
}
