package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"print-api/internal/oas"
)

func createIPPResponse(state, queued int, accepting byte) []byte {
	var body bytes.Buffer
	body.Write([]byte{1, 1, 0, 0, 0, 0, 0, 1, 4})
	attrs := []struct {
		tag   byte
		name  string
		value []byte
	}{
		{0x23, "printer-state", binary.BigEndian.AppendUint32(nil, uint32(state))},
		{0x21, "queued-job-count", binary.BigEndian.AppendUint32(nil, uint32(queued))},
		{0x22, "printer-is-accepting-jobs", []byte{accepting}},
		{0x44, "printer-state-reasons", []byte("none")},
		{0x44, "", []byte("connecting-to-device")},
	}
	for _, attr := range attrs {
		body.WriteByte(attr.tag)
		_ = binary.Write(&body, binary.BigEndian, uint16(len(attr.name)))
		body.WriteString(attr.name)
		_ = binary.Write(&body, binary.BigEndian, uint16(len(attr.value)))
		body.Write(attr.value)
	}
	body.WriteByte(3)
	return body.Bytes()
}

func TestBuildReadOnlyPrinterProbe(t *testing.T) {
	body := buildPrinterStateRequest("ipp://printer:631/ipp/print")
	if !bytes.Equal(body[:9], []byte{1, 1, 0, 11, 0, 0, 0, 1, 1}) || body[len(body)-1] != 3 {
		t.Fatal("not Get-Printer-Attributes")
	}
	for _, name := range []string{"printer-uri", "printer-state", "printer-state-reasons", "printer-is-accepting-jobs", "queued-job-count"} {
		if !bytes.Contains(body, []byte(name)) {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestParseIPPPrinterState(t *testing.T) {
	state, err := parsePrinterState(createIPPResponse(3, 0, 1))
	if err != nil || state.State != 3 || state.QueuedJobs != 0 || !state.AcceptingJobs || !state.Responsive || !reflect.DeepEqual(state.Reasons, []string{"none", "connecting-to-device"}) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	for _, body := range [][]byte{createIPPResponse(8, 0, 1), createIPPResponse(3, -1, 1), createIPPResponse(3, 0, 2)} {
		if _, err := parsePrinterState(body); err == nil {
			t.Fatal("invalid attribute accepted")
		}
	}
	valid := createIPPResponse(3, 0, 1)
	for end := 0; end < len(valid); end++ {
		if _, err := parsePrinterState(valid[:end]); err == nil {
			t.Fatalf("truncated response accepted at %d", end)
		}
	}
	wrongID := bytes.Clone(valid)
	wrongID[7] = 2
	if _, err := parsePrinterState(wrongID); err == nil {
		t.Fatal("incorrect request ID accepted")
	}
	state, err = parsePrinterState([]byte{1, 1, 5, 0, 0, 0, 0, 1, 3})
	if err == nil || !state.Responsive {
		t.Fatal("IPP fault must prove response but not readiness")
	}
}

func TestClassifyPrinterReadiness(t *testing.T) {
	for _, test := range []struct {
		state, queue int
		accept       bool
		cups, want   string
	}{
		{3, 0, true, "ready", "ready"}, {4, 0, true, "ready", "busy"}, {3, 1, true, "ready", "busy"},
		{3, 0, true, "busy", "busy"}, {3, 0, true, "unavailable", "starting"},
		{5, 0, true, "ready", "error"}, {3, 0, false, "ready", "error"},
	} {
		got := classifyPrinterReadiness(ippPrinterState{Responsive: true, State: test.state, QueuedJobs: test.queue, AcceptingJobs: test.accept}, test.cups)
		if got != test.want {
			t.Fatalf("%+v got=%s", test, got)
		}
	}
	if got := classifyPrinterReadiness(ippPrinterState{}, "ready"); got != "starting" {
		t.Fatalf("unresponsive printer with stale CUPS must not be ready: %s", got)
	}
}

func TestClassifyCUPSStatus(t *testing.T) {
	for _, test := range []struct{ output, want string }{
		{"printer mono is idle. enabled since now", "ready"},
		{"printer mono now printing mono-1", "busy"},
		{"printer mono is idle. disabled since now", "unavailable"},
		{"printer mono now printing mono-1. disabled", "unavailable"},
		{"printer mono not accepting requests", "unavailable"},
		{"unexpected", "unknown"},
	} {
		if got := classifyCUPSStatus([]byte(test.output), nil); got != test.want {
			t.Fatalf("%q: %s", test.output, got)
		}
	}
	if got := classifyCUPSStatus(nil, errors.New("CUPS failed")); got != "unavailable" {
		t.Fatalf("CUPS failure: %s", got)
	}
}

func TestIPPHTTPAndProtocolFailuresCannotAuthorizePrint(t *testing.T) {
	for _, test := range []struct {
		name       string
		httpStatus int
		body       []byte
		responsive bool
	}{
		{"HTTP authentication", 401, nil, false},
		{"HTTP failure", 503, nil, false},
		{"IPP fault", 200, []byte{1, 1, 5, 0, 0, 0, 0, 1, 3}, true},
		{"invalid body", 200, []byte("not IPP"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.httpStatus)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			state, err := readPrinterState(context.Background(), strings.Replace(server.URL, "http:", "ipp:", 1)+"/ipp/print")
			if err == nil || errors.Is(err, errPrinterUnreachable) || state.Responsive != test.responsive {
				t.Fatalf("state=%+v err=%v", state, err)
			}
		})
	}
}

func TestReadIPPPrinterStateOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/ipp" || !bytes.Equal(body[:4], []byte{1, 1, 0, 11}) {
			t.Error("not a read-only IPP request")
		}
		_, _ = w.Write(createIPPResponse(3, 0, 1))
	}))
	uri := strings.Replace(server.URL, "http:", "ipp:", 1) + "/ipp/print"
	state, err := readPrinterState(context.Background(), uri)
	if err != nil || !state.Responsive {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	server.Close()
	_, err = readPrinterState(context.Background(), uri)
	if !errors.Is(err, errPrinterUnreachable) {
		t.Fatalf("connection failure=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readPrinterState(ctx, uri)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation=%v", err)
	}
}

func TestPrinterReadinessAPIOverridesStaleCUPSAndPreservesFaults(t *testing.T) {
	for _, test := range []struct {
		name  string
		state ippPrinterState
		err   error
		want  oas.PrinterStatusStatus
	}{
		{"unreachable", ippPrinterState{}, errPrinterUnreachable, oas.PrinterStatusStatusStarting},
		{"idle", ippPrinterState{Responsive: true, State: 3, AcceptingJobs: true}, nil, oas.PrinterStatusStatusReady},
		{"busy", ippPrinterState{Responsive: true, State: 4, AcceptingJobs: true, QueuedJobs: 1}, nil, oas.PrinterStatusStatusBusy},
		{"fault", ippPrinterState{Responsive: true, State: 5, AcceptingJobs: true, Reasons: []string{"media-empty"}}, nil, oas.PrinterStatusStatusError},
		{"malformed", ippPrinterState{}, errors.New("invalid IPP state"), oas.PrinterStatusStatusError},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Printers[0].IPPURI = "ipp://printer:631/ipp/print"
			h := newAPIHandler(cfg, "secret")
			h.runLPStat = func(context.Context, ...string) ([]byte, error) {
				return []byte("printer mono-queue is idle. enabled since now"), nil
			}
			h.readPrinter = func(_ context.Context, uri string) (ippPrinterState, error) {
				if uri != cfg.Printers[0].IPPURI {
					t.Fatal("wrong binding")
				}
				return test.state, test.err
			}
			api, err := oas.NewServer(h, h)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/printers/mono/status", nil)
			r.Header.Set("X-Api-Key", "secret")
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			var payload struct {
				Status     oas.PrinterStatusStatus
				Responsive bool
				CupsStatus string `json:"cups_status"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload.Status != test.want || payload.Responsive != test.state.Responsive || payload.CupsStatus != "ready" {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), cfg.Printers[0].IPPURI) {
				t.Fatal("endpoint details leaked into response")
			}
		})
	}
}

func TestIPPConfigRejectsUnsafeURIs(t *testing.T) {
	for _, uri := range []string{"http://printer/ipp/print", "ipp:///ipp/print", "ipp://user:password@printer/ipp/print", "ipp://printer/ipp/print?key=value", "ipp://printer/ipp/print#fragment"} {
		cfg := testConfig()
		cfg.Printers[0].IPPURI = uri
		if err := cfg.validate(); err == nil {
			t.Fatalf("invalid URI accepted: %q", uri)
		}
	}
}

func FuzzParsePrinterState(f *testing.F) {
	f.Add(createIPPResponse(3, 0, 1))
	f.Add([]byte{1, 1, 5, 0, 0, 0, 0, 1, 3})
	f.Fuzz(func(t *testing.T, body []byte) {
		state, err := parsePrinterState(body)
		if err == nil && (!state.Responsive || state.State < 3 || state.State > 5 || state.QueuedJobs < 0) {
			t.Fatalf("invalid successful state %+v", state)
		}
	})
}
