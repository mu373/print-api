package main

import (
	"strings"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got, want := len(cfg.Printers), 2; got != want {
		t.Fatalf("len(Printers) = %d, want %d", got, want)
	}
	if got, want := cfg.DefaultPrinterID, "office-mono"; got != want {
		t.Fatalf("DefaultPrinterID = %q, want %q", got, want)
	}
	if got, want := len(cfg.Presets), 1; got != want {
		t.Fatalf("len(Presets) = %d, want %d", got, want)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config)
		wantErr string
	}{
		{
			name: "duplicate printer",
			mutate: func(cfg *config) {
				cfg.Printers = append(cfg.Printers, cfg.Printers[0])
			},
			wantErr: "duplicate printer id",
		},
		{
			name: "unknown default",
			mutate: func(cfg *config) {
				cfg.DefaultPrinterID = "missing"
			},
			wantErr: "is not configured",
		},
		{
			name: "unsupported edge options",
			mutate: func(cfg *config) {
				cfg.Printers[0].Capabilities.EdgeToEdge = "unsupported"
			},
			wantErr: "edge_to_edge_options must be empty",
		},
		{
			name: "missing auto color",
			mutate: func(cfg *config) {
				cfg.Printers[0].Capabilities.ColorModes = []string{"monochrome"}
			},
			wantErr: "must include auto",
		},
		{
			name: "unknown preset printer",
			mutate: func(cfg *config) {
				cfg.Presets[0].PrinterID = "missing"
			},
			wantErr: "printer_id \"missing\" is not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.mutate(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func testConfig() config {
	edgeToEdge := true
	return config{
		ListenAddr:       ":0",
		MaxUploadBytes:   1024,
		DefaultPrinterID: "mono",
		Presets: []presetConfig{
			{
				ID:         "letter-duplex-full",
				Duplex:     "long-edge",
				Media:      "letter",
				Scaling:    "none",
				EdgeToEdge: &edgeToEdge,
			},
		},
		Printers: []printerConfig{
			{
				ID:              "mono",
				DisplayName:     "Mono Printer",
				CUPSDestination: "mono-queue",
				Capabilities: capabilitiesConfig{
					ColorModes:   []string{"auto", "monochrome"},
					DuplexModes:  []string{"none", "long-edge", "short-edge"},
					Media:        []string{"a4", "letter"},
					ScalingModes: []string{"auto", "auto-fit", "fill", "fit", "none"},
					MaxCopies:    10,
					EdgeToEdge:   "printable-area",
				},
				EdgeToEdgeOptions: map[string]string{
					"page-bottom": "0",
					"page-left":   "0",
					"page-right":  "0",
					"page-top":    "0",
				},
			},
			{
				ID:              "color",
				DisplayName:     "Color Printer",
				CUPSDestination: "color-queue",
				Capabilities: capabilitiesConfig{
					ColorModes:   []string{"auto", "color", "monochrome"},
					DuplexModes:  []string{"none", "long-edge"},
					Media:        []string{"a4"},
					ScalingModes: []string{"auto", "auto-fit", "fill", "fit", "none"},
					MaxCopies:    5,
					EdgeToEdge:   "full-bleed",
				},
				EdgeToEdgeOptions: map[string]string{
					"page-bottom": "0",
					"page-left":   "0",
					"page-right":  "0",
					"page-top":    "0",
				},
			},
		},
	}
}
