package contracts_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestJSONRoundTrip(t *testing.T) {
	timestamp := time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		input     interface{}
		newOutput func() interface{}
	}{
		{
			name: "printer_health",
			input: contracts.PrinterHealth{
				Online:            true,
				State:             "IDLE",
				PaperTrayStatus:   "OK",
				EstimatedSheets:   250,
				TonerLevelPercent: 90,
				DrumLifePercent:   85,
				CoverOpen:         false,
				PaperJam:          false,
				OutputBinFull:     false,
				UpdatedAt:         timestamp,
			},
			newOutput: func() interface{} { return &contracts.PrinterHealth{} },
		},
		{
			name: "print_job_command",
			input: contracts.PrintJobCommand{
				JobID:          "job-001",
				KioskID:        "kiosk-001",
				DisplayCode:    "A74X",
				DownloadURL:    "https://example.invalid/document.pdf",
				SHA256Checksum: "0123456789abcdef",
				TotalPages:     4,
				TotalSheets:    2,
				Copies:         1,
				Duplex:         contracts.DuplexLongEdge,
				StartFromPage:  1,
			},
			newOutput: func() interface{} { return &contracts.PrintJobCommand{} },
		},
		{
			name: "job_progress_event",
			input: contracts.JobProgressEvent{
				JobID:         "job-001",
				KioskID:       "kiosk-001",
				Status:        contracts.JobStatusPrinting,
				PagesPrinted:  2,
				SheetsPrinted: 1,
				Timestamp:     timestamp,
			},
			newOutput: func() interface{} { return &contracts.JobProgressEvent{} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			got := tt.newOutput()
			if err := json.Unmarshal(encoded, got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			decoded := reflect.ValueOf(got).Elem().Interface()
			if !reflect.DeepEqual(tt.input, decoded) {
				t.Fatalf(
					"round-trip mismatch:\noriginal: %#v\ndecoded:  %#v\nJSON:     %s",
					tt.input,
					decoded,
					encoded,
				)
			}
		})
	}
}
