//go:build goexperiment.jsonv2

// Command server runs the BFF web server. Routes are generated from the OpenAPI
// spec via cmd/apigen. Each route fetches upstream data and applies a compiled
// JSONata transform before responding.
//
// Upstream service URLs are configured via environment variables:
//
//	UPSTREAM_ORDERS_URL=http://orders-svc:8080/data
//	UPSTREAM_PRODUCTS_URL=http://products-svc:8080/data
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

	"github.com/gofiber/fiber/v3"
)

func main() {
	app := fiber.New(fiber.Config{
		// Use encoding/json/v2 for all JSON marshaling inside Fiber (c.JSON, etc.)
		JSONEncoder: func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
		JSONDecoder: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },

		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	})

	RegisterRoutes(app, defaultFetch)

	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	addr := listenAddr()
	log.Printf("BFF server starting on %s", addr)

	// Start Fiber in a goroutine so we can listen for shutdown signals.
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

func listenAddr() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	return fmt.Sprintf(":%s", port)
}
