package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"print-api/internal/oas"
)

var sidesByDuplex = map[oas.DuplexMode]string{
	oas.DuplexModeNone:      "one-sided",
	oas.DuplexModeLongEdge:  "two-sided-long-edge",
	oas.DuplexModeShortEdge: "two-sided-short-edge",
}

type lpCommand func(context.Context, ...string) ([]byte, error)

type lpstatCommand func(context.Context, ...string) ([]byte, error)

type apiHandler struct {
	config    config
	apiKey    string
	printers  map[string]printerConfig
	presets   map[string]presetConfig
	runLP     lpCommand
	runLPStat lpstatCommand
}

func newAPIHandler(cfg config, apiKey string) *apiHandler {
	printers := make(map[string]printerConfig, len(cfg.Printers))
	for _, printer := range cfg.Printers {
		printers[printer.ID] = printer
	}
	presets := make(map[string]presetConfig, len(cfg.Presets))
	for _, preset := range cfg.Presets {
		presets[preset.ID] = preset
	}
	return &apiHandler{
		config:    cfg,
		apiKey:    apiKey,
		printers:  printers,
		presets:   presets,
		runLP:     runLP,
		runLPStat: runLPStat,
	}
}

func runLP(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "lp", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

func runLPStat(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "lpstat", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

func (h *apiHandler) HealthCheck(context.Context) (*oas.HealthStatus, error) {
	return &oas.HealthStatus{Status: oas.HealthStatusStatusOk}, nil
}

func (h *apiHandler) ListPresets(context.Context) (*oas.PrintPresetList, error) {
	presets := make([]oas.PrintPreset, 0, len(h.config.Presets))
	for _, preset := range h.config.Presets {
		value := oas.PrintPreset{ID: preset.ID}
		if preset.PrinterID != "" {
			value.PrinterID = oas.NewOptString(preset.PrinterID)
		}
		if preset.Copies != 0 {
			value.Copies = oas.NewOptInt(preset.Copies)
		}
		if preset.Duplex != "" {
			value.Duplex = oas.NewOptDuplexMode(oas.DuplexMode(preset.Duplex))
		}
		if preset.ColorMode != "" {
			value.ColorMode = oas.NewOptColorMode(oas.ColorMode(preset.ColorMode))
		}
		if preset.Media != "" {
			value.Media = oas.NewOptString(preset.Media)
		}
		if preset.Scaling != "" {
			value.Scaling = oas.NewOptScalingMode(oas.ScalingMode(preset.Scaling))
		}
		if preset.EdgeToEdge != nil {
			value.EdgeToEdge = oas.NewOptBool(*preset.EdgeToEdge)
		}
		presets = append(presets, value)
	}
	return &oas.PrintPresetList{Presets: presets}, nil
}

func (h *apiHandler) ListPrinters(context.Context) (*oas.PrinterList, error) {
	printers := make([]oas.Printer, 0, len(h.config.Printers))
	for _, printer := range h.config.Printers {
		colors := make([]oas.ColorMode, len(printer.Capabilities.ColorModes))
		for i, value := range printer.Capabilities.ColorModes {
			colors[i] = oas.ColorMode(value)
		}
		duplex := make([]oas.DuplexMode, len(printer.Capabilities.DuplexModes))
		for i, value := range printer.Capabilities.DuplexModes {
			duplex[i] = oas.DuplexMode(value)
		}
		scaling := make([]oas.ScalingMode, len(printer.Capabilities.ScalingModes))
		for i, value := range printer.Capabilities.ScalingModes {
			scaling[i] = oas.ScalingMode(value)
		}

		printers = append(printers, oas.Printer{
			ID:          printer.ID,
			DisplayName: printer.DisplayName,
			Default:     printer.ID == h.config.DefaultPrinterID,
			Capabilities: oas.PrinterCapabilities{
				ColorModes:   colors,
				DuplexModes:  duplex,
				Media:        append([]string(nil), printer.Capabilities.Media...),
				ScalingModes: scaling,
				EdgeToEdge:   oas.EdgeToEdgeCapability(printer.Capabilities.EdgeToEdge),
				MaxCopies:    printer.Capabilities.MaxCopies,
			},
		})
	}
	return &oas.PrinterList{Printers: printers}, nil
}

func (h *apiHandler) GetPrinterStatus(ctx context.Context, params oas.GetPrinterStatusParams) (*oas.PrinterStatus, error) {
	printer, ok := h.printers[params.PrinterID]
	if !ok {
		return printerStatus(params.PrinterID, oas.PrinterStatusStatusUnavailable, "printer is not configured"), nil
	}

	out, err := h.runLPStat(ctx, "-p", printer.CUPSDestination, "-l")
	if err != nil {
		log.Printf("lpstat failed for printer %q (%s): %v: %s", printer.ID, printer.CUPSDestination, err, strings.TrimSpace(string(out)))
		return printerStatus(printer.ID, oas.PrinterStatusStatusUnavailable, "CUPS cannot currently use this printer destination"), nil
	}

	lower := strings.ToLower(string(out))
	switch {
	case strings.Contains(lower, "now printing"):
		return printerStatus(printer.ID, oas.PrinterStatusStatusBusy, "CUPS is printing a job"), nil
	case strings.Contains(lower, "is idle") && !strings.Contains(lower, "disabled"):
		return printerStatus(printer.ID, oas.PrinterStatusStatusReady, "CUPS destination is enabled and idle"), nil
	case strings.Contains(lower, "disabled") || strings.Contains(lower, "not accepting"):
		return printerStatus(printer.ID, oas.PrinterStatusStatusUnavailable, "CUPS destination is disabled or not accepting jobs"), nil
	default:
		return printerStatus(printer.ID, oas.PrinterStatusStatusUnknown, "CUPS returned an unrecognized printer state"), nil
	}
}

func (h *apiHandler) GetPrintJob(ctx context.Context, params oas.GetPrintJobParams) (*oas.PrintJobState, error) {
	jobID := params.JobID
	printer, ok := h.printerForJobID(jobID)
	if !ok {
		return printJobState(jobID, "", oas.PrintJobStateStatusUnknown, "job ID does not belong to a configured printer"), nil
	}

	active, activeErr := h.runLPStat(ctx, "-W", "not-completed", "-o")
	if activeErr == nil && containsJobID(string(active), jobID) {
		printerState, printerErr := h.runLPStat(ctx, "-p", printer.CUPSDestination)
		if printerErr == nil && strings.Contains(strings.ToLower(string(printerState)), "now printing "+strings.ToLower(jobID)) {
			return printJobState(jobID, printer.ID, oas.PrintJobStateStatusProcessing, "CUPS is processing the job"), nil
		}
		return printJobState(jobID, printer.ID, oas.PrintJobStateStatusQueued, "CUPS has accepted the job"), nil
	}

	completed, completedErr := h.runLPStat(ctx, "-W", "completed", "-l", "-o")
	if completedErr == nil && containsJobID(string(completed), jobID) {
		lower := strings.ToLower(string(completed))
		switch {
		case strings.Contains(lower, "canceled"):
			return printJobState(jobID, printer.ID, oas.PrintJobStateStatusCanceled, "CUPS recorded the job as canceled"), nil
		case strings.Contains(lower, "aborted") || strings.Contains(lower, "stopped"):
			return printJobState(jobID, printer.ID, oas.PrintJobStateStatusAborted, "CUPS recorded the job as aborted"), nil
		default:
			return printJobState(jobID, printer.ID, oas.PrintJobStateStatusCompleted, "CUPS recorded the job as completed"), nil
		}
	}

	return printJobState(jobID, printer.ID, oas.PrintJobStateStatusUnknown, "CUPS has no current or retained record for this job"), nil
}

func (h *apiHandler) PrintDocument(ctx context.Context, req *oas.PrintDocumentReq) (oas.PrintDocumentRes, error) {
	options := resolvedPrintOptions{
		printerID: h.config.DefaultPrinterID,
		copies:    1,
		duplex:    oas.DuplexModeNone,
		colorMode: oas.ColorModeAuto,
		scaling:   oas.ScalingModeAuto,
	}
	if presetID, ok := req.Preset.Get(); ok {
		preset, exists := h.presets[presetID]
		if !exists {
			return badRequest("unknown_preset", fmt.Sprintf("preset %q is not configured", presetID)), nil
		}
		options.applyPreset(preset)
	}
	options.applyRequest(req)

	printerID := options.printerID
	printer, ok := h.printers[printerID]
	if !ok {
		return badRequest("unknown_printer", fmt.Sprintf("printer %q is not configured", printerID)), nil
	}

	copies := options.copies
	if copies < 1 || copies > printer.Capabilities.MaxCopies {
		return badRequest(
			"unsupported_copies",
			fmt.Sprintf("printer %q accepts between 1 and %d copies", printerID, printer.Capabilities.MaxCopies),
		), nil
	}

	duplex := options.duplex
	if !contains(printer.Capabilities.DuplexModes, string(duplex)) {
		return badRequest("unsupported_duplex", fmt.Sprintf("printer %q does not support duplex mode %q", printerID, duplex)), nil
	}

	colorMode := options.colorMode
	if !contains(printer.Capabilities.ColorModes, string(colorMode)) {
		return badRequest("unsupported_color_mode", fmt.Sprintf("printer %q does not support color mode %q", printerID, colorMode)), nil
	}

	media, mediaSet := options.media, options.mediaSet
	if mediaSet && !contains(printer.Capabilities.Media, media) {
		return badRequest("unsupported_media", fmt.Sprintf("printer %q does not support media %q", printerID, media)), nil
	}

	scaling := options.scaling
	if !contains(printer.Capabilities.ScalingModes, string(scaling)) {
		return badRequest("unsupported_scaling", fmt.Sprintf("printer %q does not support scaling mode %q", printerID, scaling)), nil
	}

	edgeToEdge := options.edgeToEdge
	if edgeToEdge && printer.Capabilities.EdgeToEdge == "unsupported" {
		return badRequest("unsupported_edge_to_edge", fmt.Sprintf("printer %q does not support edge-to-edge printing", printerID)), nil
	}

	if req.File.Size > h.config.MaxUploadBytes {
		return fileTooLarge(h.config.MaxUploadBytes), nil
	}

	filePath, err := copyPrintFile(req.File.File, h.config.MaxUploadBytes)
	if err != nil {
		if errors.Is(err, errFileTooLarge) {
			return fileTooLarge(h.config.MaxUploadBytes), nil
		}
		if errors.Is(err, errEmptyDocument) {
			return badRequest("empty_document", "document must not be empty"), nil
		}
		if errors.Is(err, errInvalidPDF) {
			return badRequest("invalid_document", "document must be a PDF"), nil
		}
		return internalError("store_document", "failed to store uploaded document"), nil
	}
	defer os.Remove(filePath)

	args := []string{
		"-d", printer.CUPSDestination,
		"-n", strconv.Itoa(copies),
		"-o", "sides=" + sidesByDuplex[duplex],
	}
	if colorMode != oas.ColorModeAuto {
		args = append(args, "-o", "print-color-mode="+string(colorMode))
	}
	if mediaSet {
		args = append(args, "-o", "media="+media)
	}
	if scaling != oas.ScalingModeAuto {
		args = append(args, "-o", "print-scaling="+string(scaling))
	}
	if edgeToEdge {
		optionNames := make([]string, 0, len(printer.EdgeToEdgeOptions))
		for name := range printer.EdgeToEdgeOptions {
			optionNames = append(optionNames, name)
		}
		sort.Strings(optionNames)
		for _, name := range optionNames {
			args = append(args, "-o", name+"="+printer.EdgeToEdgeOptions[name])
		}
	}
	args = append(args, filePath)

	out, err := h.runLP(ctx, args...)
	if err != nil {
		log.Printf("lp failed for printer %q (%s): %v: %s", printer.ID, printer.CUPSDestination, err, strings.TrimSpace(string(out)))
		return internalError("cups_error", "CUPS rejected the print job"), nil
	}

	return &oas.PrintJob{
		JobID:     parseJobID(string(out)),
		PrinterID: printer.ID,
		Status:    oas.PrintJobStatusQueued,
	}, nil
}

type resolvedPrintOptions struct {
	printerID  string
	copies     int
	duplex     oas.DuplexMode
	colorMode  oas.ColorMode
	media      string
	mediaSet   bool
	scaling    oas.ScalingMode
	edgeToEdge bool
}

func (o *resolvedPrintOptions) applyPreset(preset presetConfig) {
	if preset.PrinterID != "" {
		o.printerID = preset.PrinterID
	}
	if preset.Copies != 0 {
		o.copies = preset.Copies
	}
	if preset.Duplex != "" {
		o.duplex = oas.DuplexMode(preset.Duplex)
	}
	if preset.ColorMode != "" {
		o.colorMode = oas.ColorMode(preset.ColorMode)
	}
	if preset.Media != "" {
		o.media = preset.Media
		o.mediaSet = true
	}
	if preset.Scaling != "" {
		o.scaling = oas.ScalingMode(preset.Scaling)
	}
	if preset.EdgeToEdge != nil {
		o.edgeToEdge = *preset.EdgeToEdge
	}
}

func (o *resolvedPrintOptions) applyRequest(req *oas.PrintDocumentReq) {
	if value, ok := req.PrinterID.Get(); ok {
		o.printerID = value
	}
	if value, ok := req.Copies.Get(); ok {
		o.copies = value
	}
	if value, ok := req.Duplex.Get(); ok {
		o.duplex = value
	}
	if value, ok := req.ColorMode.Get(); ok {
		o.colorMode = value
	}
	if value, ok := req.Media.Get(); ok {
		o.media = value
		o.mediaSet = true
	}
	if value, ok := req.Scaling.Get(); ok {
		o.scaling = value
	}
	if value, ok := req.EdgeToEdge.Get(); ok {
		o.edgeToEdge = value
	}
}

var (
	errEmptyDocument = fmt.Errorf("empty document")
	errFileTooLarge  = fmt.Errorf("file too large")
	errInvalidPDF    = fmt.Errorf("invalid PDF")
)

func copyPrintFile(source io.Reader, maxBytes int64) (path string, err error) {
	tmp, err := os.CreateTemp("", "print-*.pdf")
	if err != nil {
		return "", err
	}
	path = tmp.Name()
	keep := false
	defer func() {
		if closeErr := tmp.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if !keep {
			_ = os.Remove(path)
		}
	}()

	written, err := io.Copy(tmp, io.LimitReader(source, maxBytes+1))
	if err != nil {
		return "", err
	}
	if written > maxBytes {
		return "", errFileTooLarge
	}
	if written == 0 {
		return "", errEmptyDocument
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	header := make([]byte, len("%PDF-"))
	if _, err := io.ReadFull(tmp, header); err != nil || string(header) != "%PDF-" {
		return "", errInvalidPDF
	}
	keep = true
	return path, nil
}

func parseJobID(output string) string {
	fields := strings.Fields(output)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "is" {
			return strings.TrimSuffix(fields[i+1], ".")
		}
	}
	return strings.TrimSpace(output)
}

func (h *apiHandler) printerForJobID(jobID string) (printerConfig, bool) {
	for _, printer := range h.config.Printers {
		prefix := printer.CUPSDestination + "-"
		if !strings.HasPrefix(jobID, prefix) {
			continue
		}
		sequence := strings.TrimPrefix(jobID, prefix)
		if sequence == "" {
			return printerConfig{}, false
		}
		if _, err := strconv.ParseUint(sequence, 10, 64); err == nil {
			return printer, true
		}
	}
	return printerConfig{}, false
}

func containsJobID(output, jobID string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == jobID {
			return true
		}
	}
	return false
}

func printerStatus(id string, status oas.PrinterStatusStatus, message string) *oas.PrinterStatus {
	result := &oas.PrinterStatus{PrinterID: id, Status: status}
	if message != "" {
		result.Message = oas.NewOptString(message)
	}
	return result
}

func printJobState(jobID, printerID string, status oas.PrintJobStateStatus, message string) *oas.PrintJobState {
	result := &oas.PrintJobState{JobID: jobID, Status: status}
	if printerID != "" {
		result.PrinterID = oas.NewOptString(printerID)
	}
	if message != "" {
		result.Message = oas.NewOptString(message)
	}
	return result
}

func badRequest(code, message string) *oas.PrintDocumentBadRequest {
	return (*oas.PrintDocumentBadRequest)(&oas.Error{Code: code, Message: message})
}

func fileTooLarge(maxBytes int64) *oas.PrintDocumentRequestEntityTooLarge {
	return (*oas.PrintDocumentRequestEntityTooLarge)(&oas.Error{
		Code:    "file_too_large",
		Message: fmt.Sprintf("document exceeds the %d byte upload limit", maxBytes),
	})
}

func internalError(code, message string) *oas.PrintDocumentInternalServerError {
	return (*oas.PrintDocumentInternalServerError)(&oas.Error{Code: code, Message: message})
}
