package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	ht "github.com/ogen-go/ogen/http"

	"print-api/internal/oas"
)

func TestPrintDocumentBuildsGenericCUPSOptions(t *testing.T) {
	cfg := testConfig()
	handler := newAPIHandler(cfg, "secret")

	var gotArgs []string
	var tempPath string
	handler.runLP = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		tempPath = args[len(args)-1]
		contents, err := os.ReadFile(tempPath)
		if err != nil {
			t.Fatalf("read temporary print file: %v", err)
		}
		if got, want := string(contents), "%PDF-1.7\ntest"; got != want {
			t.Fatalf("temporary file = %q, want %q", got, want)
		}
		return []byte("request id is color-queue-42 (1 file(s))\n"), nil
	}

	req := &oas.PrintDocumentReq{
		File:       ht.MultipartFile{Name: "test.pdf", File: strings.NewReader("%PDF-1.7\ntest"), Size: 13},
		PrinterID:  oas.NewOptString("color"),
		Copies:     oas.NewOptInt(2),
		Duplex:     oas.NewOptDuplexMode(oas.DuplexModeLongEdge),
		ColorMode:  oas.NewOptColorMode(oas.ColorModeColor),
		Media:      oas.NewOptString("a4"),
		Scaling:    oas.NewOptScalingMode(oas.ScalingModeFill),
		EdgeToEdge: oas.NewOptBool(true),
	}
	res, err := handler.PrintDocument(context.Background(), req)
	if err != nil {
		t.Fatalf("PrintDocument() error = %v", err)
	}
	job, ok := res.(*oas.PrintJob)
	if !ok {
		t.Fatalf("PrintDocument() response type = %T, want *oas.PrintJob", res)
	}
	if got, want := job.JobID, "color-queue-42"; got != want {
		t.Fatalf("JobID = %q, want %q", got, want)
	}

	wantOptions := []string{
		"sides=two-sided-long-edge",
		"print-color-mode=color",
		"media=a4",
		"page-bottom=0",
		"page-left=0",
		"page-right=0",
		"page-top=0",
		"print-scaling=fill",
	}
	if len(gotArgs) < 5 || gotArgs[0] != "-d" || gotArgs[1] != "color-queue" || gotArgs[2] != "-n" || gotArgs[3] != "2" {
		t.Fatalf("lp destination/copies args = %v", gotArgs)
	}
	for _, option := range wantOptions {
		if !containsOption(gotArgs, option) {
			t.Errorf("lp args %v do not contain option %q", gotArgs, option)
		}
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists after printing: %v", err)
	}
}

func TestPrintDocumentRejectsUnsupportedCapability(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	called := false
	handler.runLP = func(context.Context, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	res, err := handler.PrintDocument(context.Background(), &oas.PrintDocumentReq{
		File:      ht.MultipartFile{Name: "test.pdf", File: strings.NewReader("%PDF-1.7"), Size: 8},
		PrinterID: oas.NewOptString("mono"),
		ColorMode: oas.NewOptColorMode(oas.ColorModeColor),
	})
	if err != nil {
		t.Fatalf("PrintDocument() error = %v", err)
	}
	badRequest, ok := res.(*oas.PrintDocumentBadRequest)
	if !ok {
		t.Fatalf("PrintDocument() response type = %T, want *oas.PrintDocumentBadRequest", res)
	}
	if got, want := badRequest.Code, "unsupported_color_mode"; got != want {
		t.Fatalf("error code = %q, want %q", got, want)
	}
	if called {
		t.Fatal("lp was called for an unsupported color mode")
	}
}

func TestPrintDocumentAutoColorUsesPrinterDefault(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	var gotArgs []string
	handler.runLP = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("request id is mono-queue-1 (1 file(s))\n"), nil
	}

	res, err := handler.PrintDocument(context.Background(), &oas.PrintDocumentReq{
		File: ht.MultipartFile{Name: "test.pdf", File: strings.NewReader("%PDF-1.7"), Size: 8},
	})
	if err != nil {
		t.Fatalf("PrintDocument() error = %v", err)
	}
	if _, ok := res.(*oas.PrintJob); !ok {
		t.Fatalf("PrintDocument() response type = %T, want *oas.PrintJob", res)
	}
	for _, arg := range gotArgs {
		if strings.HasPrefix(arg, "print-color-mode=") {
			t.Fatalf("auto color mode should use the printer default; args = %v", gotArgs)
		}
		if strings.HasPrefix(arg, "print-scaling=") {
			t.Fatalf("auto scaling should use the printer default; args = %v", gotArgs)
		}
	}
}

func TestPrintDocumentAppliesPresetAndExplicitOverrides(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	var gotArgs []string
	handler.runLP = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("request id is mono-queue-2 (1 file(s))\n"), nil
	}

	res, err := handler.PrintDocument(context.Background(), &oas.PrintDocumentReq{
		File:      ht.MultipartFile{Name: "test.pdf", File: strings.NewReader("%PDF-1.7"), Size: 8},
		Preset:    oas.NewOptString("letter-duplex-full"),
		Copies:    oas.NewOptInt(2),
		ColorMode: oas.NewOptColorMode(oas.ColorModeMonochrome),
	})
	if err != nil {
		t.Fatalf("PrintDocument() error = %v", err)
	}
	if _, ok := res.(*oas.PrintJob); !ok {
		t.Fatalf("PrintDocument() response type = %T, want *oas.PrintJob", res)
	}
	for _, option := range []string{
		"sides=two-sided-long-edge",
		"print-color-mode=monochrome",
		"media=letter",
		"print-scaling=none",
		"page-bottom=0",
		"page-left=0",
		"page-right=0",
		"page-top=0",
	} {
		if !containsOption(gotArgs, option) {
			t.Errorf("preset lp args %v do not contain option %q", gotArgs, option)
		}
	}
	if len(gotArgs) < 4 || gotArgs[2] != "-n" || gotArgs[3] != "2" {
		t.Fatalf("explicit copies did not override defaults: %v", gotArgs)
	}
}

func TestListPresets(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	result, err := handler.ListPresets(context.Background())
	if err != nil {
		t.Fatalf("ListPresets() error = %v", err)
	}
	if got, want := len(result.Presets), 1; got != want {
		t.Fatalf("len(Presets) = %d, want %d", got, want)
	}
	preset := result.Presets[0]
	if got, want := preset.ID, "letter-duplex-full"; got != want {
		t.Fatalf("preset ID = %q, want %q", got, want)
	}
	if got, ok := preset.Scaling.Get(); !ok || got != oas.ScalingModeNone {
		t.Fatalf("preset scaling = %q, %v; want none, true", got, ok)
	}
}

func TestGetPrinterStatus(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	handler.runLPStat = func(_ context.Context, args ...string) ([]byte, error) {
		if got, want := strings.Join(args, " "), "-p mono-queue -l"; got != want {
			t.Fatalf("lpstat args = %q, want %q", got, want)
		}
		return []byte("printer mono-queue is idle. enabled since now\n"), nil
	}

	result, err := handler.GetPrinterStatus(context.Background(), oas.GetPrinterStatusParams{PrinterID: "mono"})
	if err != nil {
		t.Fatalf("GetPrinterStatus() error = %v", err)
	}
	if got, want := result.Status, oas.PrinterStatusStatusReady; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

func TestGetPrinterStatusUnavailableWhenCUPSRejectsDestination(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	handler.runLPStat = func(context.Context, ...string) ([]byte, error) {
		return []byte("lpstat: Unknown destination\n"), errors.New("exit status 1")
	}

	result, err := handler.GetPrinterStatus(context.Background(), oas.GetPrinterStatusParams{PrinterID: "mono"})
	if err != nil {
		t.Fatalf("GetPrinterStatus() error = %v", err)
	}
	if got, want := result.Status, oas.PrinterStatusStatusUnavailable; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

func TestGetPrintJobState(t *testing.T) {
	t.Run("processing", func(t *testing.T) {
		handler := newAPIHandler(testConfig(), "secret")
		handler.runLPStat = func(_ context.Context, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "-W not-completed -o":
				return []byte("mono-queue-7 minami 1024 now\n"), nil
			case "-p mono-queue":
				return []byte("printer mono-queue now printing mono-queue-7. enabled since now\n"), nil
			default:
				t.Fatalf("unexpected lpstat args: %v", args)
				return nil, nil
			}
		}
		result, err := handler.GetPrintJob(context.Background(), oas.GetPrintJobParams{JobID: "mono-queue-7"})
		if err != nil {
			t.Fatalf("GetPrintJob() error = %v", err)
		}
		if got, want := result.Status, oas.PrintJobStateStatusProcessing; got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	})

	t.Run("completed", func(t *testing.T) {
		handler := newAPIHandler(testConfig(), "secret")
		handler.runLPStat = func(_ context.Context, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "-W not-completed -o":
				return nil, errors.New("not found")
			case "-W completed -l -o":
				return []byte("mono-queue-7 minami 1024 completed at now\n"), nil
			default:
				t.Fatalf("unexpected lpstat args: %v", args)
				return nil, nil
			}
		}
		result, err := handler.GetPrintJob(context.Background(), oas.GetPrintJobParams{JobID: "mono-queue-7"})
		if err != nil {
			t.Fatalf("GetPrintJob() error = %v", err)
		}
		if got, want := result.Status, oas.PrintJobStateStatusCompleted; got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	})

	t.Run("unknown printer", func(t *testing.T) {
		handler := newAPIHandler(testConfig(), "secret")
		result, err := handler.GetPrintJob(context.Background(), oas.GetPrintJobParams{JobID: "other-queue-7"})
		if err != nil {
			t.Fatalf("GetPrintJob() error = %v", err)
		}
		if got, want := result.Status, oas.PrintJobStateStatusUnknown; got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	})
}

func TestPrintDocumentRejectsNonPDF(t *testing.T) {
	handler := newAPIHandler(testConfig(), "secret")
	handler.runLP = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("lp was called for a non-PDF document")
		return nil, nil
	}

	res, err := handler.PrintDocument(context.Background(), &oas.PrintDocumentReq{
		File: ht.MultipartFile{Name: "test.txt", File: strings.NewReader("hello"), Size: 5},
	})
	if err != nil {
		t.Fatalf("PrintDocument() error = %v", err)
	}
	badRequest, ok := res.(*oas.PrintDocumentBadRequest)
	if !ok {
		t.Fatalf("PrintDocument() response type = %T, want *oas.PrintDocumentBadRequest", res)
	}
	if got, want := badRequest.Code, "invalid_document"; got != want {
		t.Fatalf("error code = %q, want %q", got, want)
	}
}

func TestGeneratedHTTPServer(t *testing.T) {
	cfg := testConfig()
	handler := newAPIHandler(cfg, "secret")
	handler.runLP = func(context.Context, ...string) ([]byte, error) {
		return []byte("request id is mono-queue-7 (1 file(s))\n"), nil
	}
	handler.runLPStat = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "-p mono-queue -l":
			return []byte("printer mono-queue is idle. enabled since now\n"), nil
		case "-W not-completed -o":
			return nil, errors.New("not found")
		case "-W completed -l -o":
			return []byte("mono-queue-7 minami 1024 completed at now\n"), nil
		default:
			return nil, errors.New("unexpected lpstat arguments")
		}
	}
	server, err := oas.NewServer(handler, handler, oas.WithErrorHandler(apiErrorHandler))
	if err != nil {
		t.Fatalf("oas.NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(limitPrintBody(server, cfg.MaxUploadBytes))
	defer testServer.Close()

	unauthorized, err := http.Get(testServer.URL + "/printers")
	if err != nil {
		t.Fatalf("GET /printers without API key: %v", err)
	}
	defer unauthorized.Body.Close()
	if got, want := unauthorized.StatusCode, http.StatusUnauthorized; got != want {
		t.Fatalf("GET /printers status = %d, want %d", got, want)
	}

	presetsRequest, err := http.NewRequest(http.MethodGet, testServer.URL+"/presets", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	presetsRequest.Header.Set("x-api-key", "secret")
	presetsResponse, err := http.DefaultClient.Do(presetsRequest)
	if err != nil {
		t.Fatalf("GET /presets: %v", err)
	}
	defer presetsResponse.Body.Close()
	if got, want := presetsResponse.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /presets status = %d, want %d", got, want)
	}

	printerStatusRequest, err := http.NewRequest(http.MethodGet, testServer.URL+"/printers/mono/status", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	printerStatusRequest.Header.Set("x-api-key", "secret")
	printerStatusResponse, err := http.DefaultClient.Do(printerStatusRequest)
	if err != nil {
		t.Fatalf("GET /printers/mono/status: %v", err)
	}
	defer printerStatusResponse.Body.Close()
	if got, want := printerStatusResponse.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /printers/mono/status = %d, want %d", got, want)
	}

	jobStatusRequest, err := http.NewRequest(http.MethodGet, testServer.URL+"/jobs/mono-queue-7", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	jobStatusRequest.Header.Set("x-api-key", "secret")
	jobStatusResponse, err := http.DefaultClient.Do(jobStatusRequest)
	if err != nil {
		t.Fatalf("GET /jobs/mono-queue-7: %v", err)
	}
	defer jobStatusResponse.Body.Close()
	if got, want := jobStatusResponse.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /jobs/mono-queue-7 = %d, want %d", got, want)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "test.pdf")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	_, _ = io.WriteString(file, "%PDF-1.7\ntest")
	_ = writer.WriteField("printer_id", "mono")
	_ = writer.WriteField("duplex", "short-edge")
	_ = writer.WriteField("color_mode", "monochrome")
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, testServer.URL+"/print", &body)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", "secret")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /print: %v", err)
	}
	defer response.Body.Close()
	if got, want := response.StatusCode, http.StatusAccepted; got != want {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /print status = %d, want %d; body = %s", got, want, data)
	}
	var job oas.PrintJob
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		t.Fatalf("decode print response: %v", err)
	}
	if got, want := job.JobID, "mono-queue-7"; got != want {
		t.Fatalf("JobID = %q, want %q", got, want)
	}
}

func TestSwaggerUI(t *testing.T) {
	cfg := testConfig()
	handler := newAPIHandler(cfg, "secret")
	server, err := oas.NewServer(handler, handler, oas.WithErrorHandler(apiErrorHandler))
	if err != nil {
		t.Fatalf("oas.NewServer() error = %v", err)
	}
	httpHandler := newHTTPHandler(server, cfg.MaxUploadBytes)

	redirect := httptest.NewRecorder()
	httpHandler.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if got, want := redirect.Code, http.StatusPermanentRedirect; got != want {
		t.Fatalf("GET /docs status = %d, want %d", got, want)
	}
	if got, want := redirect.Header().Get("Location"), "/docs/"; got != want {
		t.Fatalf("GET /docs location = %q, want %q", got, want)
	}

	index := httptest.NewRecorder()
	httpHandler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/docs/", nil))
	if got, want := index.Code, http.StatusOK; got != want {
		t.Fatalf("GET /docs/ status = %d, want %d", got, want)
	}
	if !strings.Contains(index.Body.String(), "/openapi.yaml") {
		t.Fatalf("Swagger UI does not reference /openapi.yaml")
	}

	asset := httptest.NewRecorder()
	httpHandler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/docs/swagger-ui.css", nil))
	if got, want := asset.Code, http.StatusOK; got != want {
		t.Fatalf("GET Swagger UI asset status = %d, want %d", got, want)
	}
	if asset.Body.Len() == 0 {
		t.Fatal("embedded Swagger UI asset is empty")
	}
}

func containsOption(args []string, wanted string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" && args[i+1] == wanted {
			return true
		}
	}
	return slices.Contains(args, wanted)
}
