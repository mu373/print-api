package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var errPrinterUnreachable = errors.New("printer IPP endpoint is unreachable")

type ippPrinterState struct {
	Responsive    bool
	State         int
	AcceptingJobs bool
	QueuedJobs    int
	Reasons       []string
}

type printerProbe func(context.Context, string) (ippPrinterState, error)

func readPrinterState(parent context.Context, uri string) (ippPrinterState, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	endpoint, err := url.Parse(uri)
	if err != nil {
		return ippPrinterState{}, fmt.Errorf("invalid printer IPP endpoint")
	}
	if endpoint.Port() == "" {
		endpoint.Host = net.JoinHostPort(endpoint.Hostname(), "631")
	}
	if endpoint.Scheme == "ipps" {
		endpoint.Scheme = "https"
	} else {
		endpoint.Scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(buildPrinterStateRequest(uri)))
	if err != nil {
		return ippPrinterState{}, fmt.Errorf("cannot construct printer IPP request")
	}
	req.Header.Set("Content-Type", "application/ipp")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return ippPrinterState{}, parent.Err()
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EHOSTDOWN) || errors.Is(err, syscall.ENETUNREACH) {
			return ippPrinterState{}, errPrinterUnreachable
		}
		return ippPrinterState{}, fmt.Errorf("printer IPP connection could not be verified")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ippPrinterState{}, fmt.Errorf("printer IPP endpoint returned HTTP %d", response.StatusCode)
	}
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return ippPrinterState{}, fmt.Errorf("printer IPP response could not be read")
	}
	return parsePrinterState(body)
}

func buildPrinterStateRequest(uri string) []byte {
	var body bytes.Buffer
	body.Write([]byte{1, 1, 0, 11, 0, 0, 0, 1, 1})
	attributes := []struct {
		tag         byte
		name, value string
	}{
		{0x47, "attributes-charset", "utf-8"},
		{0x48, "attributes-natural-language", "en"},
		{0x45, "printer-uri", uri},
		{0x44, "requested-attributes", "printer-state"},
		{0x44, "", "printer-state-reasons"},
		{0x44, "", "printer-is-accepting-jobs"},
		{0x44, "", "queued-job-count"},
	}
	for _, attr := range attributes {
		body.WriteByte(attr.tag)
		_ = binary.Write(&body, binary.BigEndian, uint16(len(attr.name)))
		body.WriteString(attr.name)
		_ = binary.Write(&body, binary.BigEndian, uint16(len(attr.value)))
		body.WriteString(attr.value)
	}
	body.WriteByte(3)
	return body.Bytes()
}

// Parsing and readiness classification do not perform I/O. Validate required
// attributes instead of allowing malformed or partial replies to authorize print.
func parsePrinterState(body []byte) (ippPrinterState, error) {
	invalid := errors.New("printer returned invalid IPP state")
	state := ippPrinterState{}
	if len(body) < 9 || (body[0] != 1 && body[0] != 2) || binary.BigEndian.Uint32(body[4:8]) != 1 {
		return state, invalid
	}
	state.Responsive = true
	if code := binary.BigEndian.Uint16(body[2:4]); code > 0xff {
		return state, fmt.Errorf("printer returned IPP error 0x%04x", code)
	}
	seen := map[string]bool{}
	name := ""
	group := byte(0)
	ended := false
	for offset := 8; offset < len(body); {
		tag := body[offset]
		offset++
		if tag == 3 {
			ended = true
			break
		}
		if tag < 0x10 {
			group = tag
			name = ""
			continue
		}
		if offset+2 > len(body) {
			return state, invalid
		}
		nameLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		offset += 2
		if offset+nameLength+2 > len(body) {
			return state, invalid
		}
		if nameLength > 0 {
			name = string(body[offset : offset+nameLength])
		}
		offset += nameLength
		valueLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		offset += 2
		if offset+valueLength > len(body) || name == "" || group == 0 {
			return state, invalid
		}
		value := body[offset : offset+valueLength]
		offset += valueLength
		if group != 4 {
			continue
		}
		switch name {
		case "printer-state", "queued-job-count":
			wantTag := byte(0x21)
			if name == "printer-state" {
				wantTag = 0x23
			}
			if tag != wantTag || len(value) != 4 || seen[name] {
				return state, invalid
			}
			number := int(int32(binary.BigEndian.Uint32(value)))
			if name == "printer-state" {
				if number < 3 || number > 5 {
					return state, invalid
				}
				state.State = number
			} else {
				if number < 0 {
					return state, invalid
				}
				state.QueuedJobs = number
			}
			seen[name] = true
		case "printer-is-accepting-jobs":
			if tag != 0x22 || len(value) != 1 || value[0] > 1 || seen[name] {
				return state, invalid
			}
			state.AcceptingJobs = value[0] == 1
			seen[name] = true
		case "printer-state-reasons":
			if tag != 0x44 || len(value) == 0 {
				return state, invalid
			}
			state.Reasons = append(state.Reasons, string(value))
		}
	}
	if !ended || !seen["printer-state"] || !seen["queued-job-count"] || !seen["printer-is-accepting-jobs"] {
		return state, invalid
	}
	return state, nil
}

func classifyPrinterReadiness(state ippPrinterState, cups string) string {
	if !state.Responsive {
		return "starting"
	}
	if state.State == 5 || !state.AcceptingJobs {
		return "error"
	}
	if state.State == 4 || state.QueuedJobs > 0 || cups == "busy" {
		return "busy"
	}
	if cups == "ready" {
		return "ready"
	}
	return "starting"
}

func classifyCUPSStatus(output []byte, err error) string {
	if err != nil {
		return "unavailable"
	}
	lower := strings.ToLower(string(output))
	switch {
	case strings.Contains(lower, "disabled") || strings.Contains(lower, "not accepting"):
		return "unavailable"
	case strings.Contains(lower, "now printing"):
		return "busy"
	case strings.Contains(lower, "is idle"):
		return "ready"
	default:
		return "unknown"
	}
}
