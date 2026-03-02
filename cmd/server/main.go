//go:build goexperiment.jsonv2

// Command server runs the BFF web server. Routes are generated from config.yaml
// via cmd/apigen. Each route either fetches from a single upstream (filter mode)
// or fans out to multiple providers via the aggregator (provider mode).
//
// Provider base URLs default to config.yaml but can be overridden at runtime:
//
//	UPSTREAM_USER_SERVICE_URL=http://user-svc:8080
//	UPSTREAM_ORDERS_URL=http://orders-svc:8080/data
//
// Run:
//
//	GOEXPERIMENT=jsonv2 go run ./cmd/server/
package main

import (
	jsonv2 "encoding/json/v2"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gcossani/ssfbff/internal/aggregator"
	"github.com/gofiber/fiber/v3"
	"gopkg.in/yaml.v3"
)

func main() {
	providers, err := loadProviders("config.yaml")
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	agg := aggregator.New(providers)

	app := fiber.New(fiber.Config{
		JSONEncoder: func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
		JSONDecoder: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },

		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	})

	RegisterRoutes(app, defaultFetch, agg)

	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	addr := listenAddr()
	log.Printf("BFF server starting on %s", addr)

	go func() {
		if err := app.Listen(addr); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("received %v, shutting down...", sig)

	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("server stopped")
}

// loadProviders reads the providers section from config.yaml.
func loadProviders(path string) (map[string]aggregator.ProviderConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg struct {
		Providers map[string]aggregator.ProviderConfig `yaml:"providers"`
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg.Providers, nil
}

func listenAddr() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	return fmt.Sprintf(":%s", port)
}
