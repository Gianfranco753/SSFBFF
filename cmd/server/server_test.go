//go:build goexperiment.jsonv2

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProviders(t *testing.T) {
	dir := t.TempDir()

	user := `base_url: http://user-svc:8080
timeout: 5s
endpoints:
  profile: /api/profile
`
	bank := `base_url: http://bank-svc:8080
timeout: 3s
optional: true
endpoints:
  accounts: /api/accounts
`
	os.WriteFile(filepath.Join(dir, "user_service.yaml"), []byte(user), 0o644)
	os.WriteFile(filepath.Join(dir, "bank_service.yaml"), []byte(bank), 0o644)
	// Non-YAML file should be skipped.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore me"), 0o644)

	providers, err := loadProviders(dir)
	if err != nil {
		t.Fatalf("loadProviders error: %v", err)
	}

	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}

	userCfg, ok := providers["user_service"]
	if !ok {
		t.Fatal("missing user_service provider")
	}
	if userCfg.BaseURL != "http://user-svc:8080" {
		t.Errorf("user_service base_url = %q", userCfg.BaseURL)
	}
	if userCfg.Endpoints["profile"] != "/api/profile" {
		t.Errorf("user_service profile endpoint = %q", userCfg.Endpoints["profile"])
	}
	if userCfg.Optional {
		t.Error("user_service should not be optional")
	}

	bankCfg := providers["bank_service"]
	if !bankCfg.Optional {
		t.Error("bank_service should be optional")
	}
}

func TestLoadProvidersEmptyDir(t *testing.T) {
	dir := t.TempDir()
	providers, err := loadProviders(dir)
	if err != nil {
		t.Fatalf("loadProviders error: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers, got %d", len(providers))
	}
}

func TestLoadProvidersMissingDir(t *testing.T) {
	_, err := loadProviders("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestLoadProvidersInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("{{invalid yaml"), 0o644)

	_, err := loadProviders(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestListenAddr(t *testing.T) {
	// Default port.
	t.Setenv("PORT", "")
	if got := listenAddr(); got != ":3000" {
		t.Errorf("listenAddr() = %q, want :3000", got)
	}

	// Custom port.
	t.Setenv("PORT", "8080")
	if got := listenAddr(); got != ":8080" {
		t.Errorf("listenAddr() = %q, want :8080", got)
	}
}

// --- fetch.go tests ---

func TestDefaultFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	data, err := defaultFetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("defaultFetch error: %v", err)
	}
	if got := string(data); got != `{"status":"ok"}` {
		t.Errorf("defaultFetch = %q, want %q", got, `{"status":"ok"}`)
	}
}

func TestDefaultFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := defaultFetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestDefaultFetchBadURL(t *testing.T) {
	_, err := defaultFetch(context.Background(), "http://localhost:1") // connection refused
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}
