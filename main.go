package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/swaggest/swgui/v5emb"

	"print-api/internal/oas"
)

const defaultConfigPath = "config.json"

//go:embed openapi.yaml
var openAPISpec []byte

func main() {
	configPath := os.Getenv("PRINT_API_CONFIG")
	if configPath == "" {
		configPath = defaultConfigPath
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	apiKey := os.Getenv("PRINT_API_KEY")
	if apiKey == "" {
		log.Fatal("PRINT_API_KEY must be set")
	}

	handler := newAPIHandler(cfg, apiKey)
	server, err := oas.NewServer(
		handler,
		handler,
		oas.WithErrorHandler(apiErrorHandler),
		oas.WithMaxMultipartMemory(min(cfg.MaxUploadBytes, 8<<20)),
	)
	if err != nil {
		log.Fatalf("create OpenAPI server: %v", err)
	}

	mux := newHTTPHandler(server, cfg.MaxUploadBytes)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       1 * time.Minute,
	}

	log.Printf("listening on %s with %d configured printer(s); docs at /docs/", cfg.ListenAddr, len(cfg.Printers))
	log.Fatal(httpServer.ListenAndServe())
}

func newHTTPHandler(api http.Handler, maxUploadBytes int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openAPISpec)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusPermanentRedirect)
	})
	mux.Handle("GET /docs/", v5emb.New("Print API", "/openapi.yaml", "/docs/"))
	mux.Handle("/", limitPrintBody(api, maxUploadBytes))
	return mux
}

func limitPrintBody(next http.Handler, maxUploadBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/print" {
			// Leave room for multipart headers and non-file form fields. The file itself
			// is checked against the exact limit in the operation handler.
			r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
		}
		next.ServeHTTP(w, r)
	})
}

func apiErrorHandler(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	code := "internal_error"
	message := "internal server error"

	var maxBytesError *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesError):
		status = http.StatusRequestEntityTooLarge
		code = "file_too_large"
		message = "uploaded document is too large"
	case status == http.StatusUnauthorized:
		code = "unauthorized"
		message = "invalid or missing API key"
	case status >= 400 && status < 500:
		code = "invalid_request"
		message = err.Error()
	default:
		log.Printf("request error: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(oas.Error{Code: code, Message: message})
}

func (h *apiHandler) HandleApiKeyAuth(
	ctx context.Context,
	_ oas.OperationName,
	credentials oas.ApiKeyAuth,
) (context.Context, error) {
	want := sha256.Sum256([]byte(h.apiKey))
	got := sha256.Sum256([]byte(credentials.APIKey))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return ctx, fmt.Errorf("invalid API key")
	}
	return ctx, nil
}
