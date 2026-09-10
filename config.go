package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const defaultMaxUploadBytes int64 = 32 << 20

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	optionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type config struct {
	ListenAddr       string          `json:"listen_addr"`
	MaxUploadBytes   int64           `json:"max_upload_bytes"`
	DefaultPrinterID string          `json:"default_printer_id"`
	Presets          []presetConfig  `json:"presets,omitempty"`
	Printers         []printerConfig `json:"printers"`
}

type presetConfig struct {
	ID         string `json:"id"`
	PrinterID  string `json:"printer_id,omitempty"`
	Copies     int    `json:"copies,omitempty"`
	Duplex     string `json:"duplex,omitempty"`
	ColorMode  string `json:"color_mode,omitempty"`
	Media      string `json:"media,omitempty"`
	Scaling    string `json:"scaling,omitempty"`
	EdgeToEdge *bool  `json:"edge_to_edge,omitempty"`
}

type printerConfig struct {
	ID                string             `json:"id"`
	DisplayName       string             `json:"display_name"`
	CUPSDestination   string             `json:"cups_destination"`
	Capabilities      capabilitiesConfig `json:"capabilities"`
	EdgeToEdgeOptions map[string]string  `json:"edge_to_edge_options,omitempty"`
}

type capabilitiesConfig struct {
	ColorModes   []string `json:"color_modes"`
	DuplexModes  []string `json:"duplex_modes"`
	Media        []string `json:"media"`
	ScalingModes []string `json:"scaling_modes"`
	MaxCopies    int      `json:"max_copies"`
	EdgeToEdge   string   `json:"edge_to_edge"`
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}

	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return config{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values are not allowed")
	}
	return err
}

func (c *config) validate() error {
	if c.ListenAddr == "" {
		c.ListenAddr = ":8000"
	}
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = defaultMaxUploadBytes
	}
	if c.MaxUploadBytes < 1 {
		return fmt.Errorf("max_upload_bytes must be positive")
	}
	if len(c.Printers) == 0 {
		return fmt.Errorf("at least one printer is required")
	}

	seen := make(map[string]struct{}, len(c.Printers))
	for i := range c.Printers {
		printer := &c.Printers[i]
		if err := printer.validate(); err != nil {
			return fmt.Errorf("printers[%d]: %w", i, err)
		}
		if _, ok := seen[printer.ID]; ok {
			return fmt.Errorf("duplicate printer id %q", printer.ID)
		}
		seen[printer.ID] = struct{}{}
	}

	if c.DefaultPrinterID == "" && len(c.Printers) == 1 {
		c.DefaultPrinterID = c.Printers[0].ID
	}
	if _, ok := seen[c.DefaultPrinterID]; !ok {
		return fmt.Errorf("default_printer_id %q is not configured", c.DefaultPrinterID)
	}

	seenPresets := make(map[string]struct{}, len(c.Presets))
	for i := range c.Presets {
		preset := &c.Presets[i]
		if err := preset.validate(seen); err != nil {
			return fmt.Errorf("presets[%d]: %w", i, err)
		}
		if _, ok := seenPresets[preset.ID]; ok {
			return fmt.Errorf("duplicate preset id %q", preset.ID)
		}
		seenPresets[preset.ID] = struct{}{}
	}
	return nil
}

func (p *presetConfig) validate(printers map[string]struct{}) error {
	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("invalid id %q", p.ID)
	}
	if p.PrinterID != "" {
		if _, ok := printers[p.PrinterID]; !ok {
			return fmt.Errorf("printer_id %q is not configured", p.PrinterID)
		}
	}
	if p.Copies < 0 || p.Copies > 99 {
		return fmt.Errorf("copies must be between 1 and 99 when set")
	}
	if p.Duplex != "" && !contains([]string{"none", "long-edge", "short-edge"}, p.Duplex) {
		return fmt.Errorf("invalid duplex value %q", p.Duplex)
	}
	if p.ColorMode != "" && !contains([]string{"auto", "color", "monochrome"}, p.ColorMode) {
		return fmt.Errorf("invalid color_mode value %q", p.ColorMode)
	}
	if p.Media != "" && !safeOptionValue(p.Media) {
		return fmt.Errorf("invalid media value %q", p.Media)
	}
	if p.Scaling != "" && !contains([]string{"auto", "auto-fit", "fill", "fit", "none"}, p.Scaling) {
		return fmt.Errorf("invalid scaling value %q", p.Scaling)
	}
	return nil
}

func (p *printerConfig) validate() error {
	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("invalid id %q", p.ID)
	}
	if p.DisplayName == "" {
		p.DisplayName = p.ID
	}
	if !idPattern.MatchString(p.CUPSDestination) {
		return fmt.Errorf("invalid cups_destination %q", p.CUPSDestination)
	}
	if err := p.Capabilities.validate(); err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}

	if p.Capabilities.EdgeToEdge == "unsupported" && len(p.EdgeToEdgeOptions) != 0 {
		return fmt.Errorf("edge_to_edge_options must be empty when edge_to_edge is unsupported")
	}
	if p.Capabilities.EdgeToEdge != "unsupported" && len(p.EdgeToEdgeOptions) == 0 {
		return fmt.Errorf("edge_to_edge_options are required when edge_to_edge is supported")
	}
	for name, value := range p.EdgeToEdgeOptions {
		if !optionPattern.MatchString(name) {
			return fmt.Errorf("invalid edge-to-edge option name %q", name)
		}
		if !safeOptionValue(value) {
			return fmt.Errorf("invalid value for edge-to-edge option %q", name)
		}
	}
	return nil
}

func (c *capabilitiesConfig) validate() error {
	if c.MaxCopies < 1 || c.MaxCopies > 99 {
		return fmt.Errorf("max_copies must be between 1 and 99")
	}
	if err := validateValues("color_modes", c.ColorModes, []string{"auto", "color", "monochrome"}); err != nil {
		return err
	}
	if !contains(c.ColorModes, "auto") {
		return fmt.Errorf("color_modes must include auto")
	}
	if err := validateValues("duplex_modes", c.DuplexModes, []string{"none", "long-edge", "short-edge"}); err != nil {
		return err
	}
	if !contains(c.DuplexModes, "none") {
		return fmt.Errorf("duplex_modes must include none")
	}
	if err := validateValues("scaling_modes", c.ScalingModes, []string{"auto", "auto-fit", "fill", "fit", "none"}); err != nil {
		return err
	}
	if !contains(c.ScalingModes, "auto") {
		return fmt.Errorf("scaling_modes must include auto")
	}
	if err := validateValues("edge_to_edge", []string{c.EdgeToEdge}, []string{"unsupported", "printable-area", "full-bleed"}); err != nil {
		return err
	}
	for _, media := range c.Media {
		if !safeOptionValue(media) {
			return fmt.Errorf("invalid media value %q", media)
		}
	}
	return nil
}

func validateValues(name string, values, allowed []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s must not be empty", name)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !contains(allowed, value) {
			return fmt.Errorf("invalid %s value %q", name, value)
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate %s value %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func safeOptionValue(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n")
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
