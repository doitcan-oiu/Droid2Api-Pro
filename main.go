package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"droid2api/auth"
	"droid2api/config"
	"droid2api/handler"
	"droid2api/useragent"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	// Load configuration
	cfgPath := "config.yaml"
	if env := os.Getenv("CONFIG_PATH"); env != "" {
		cfgPath = env
	}
	if err := config.Load(cfgPath); err != nil {
		log.Fatalf("[FATAL] Failed to load config: %v", err)
	}
	c := config.Get()
	log.Printf("[INFO] Dev mode: %v", c.DevMode)

	// Initialize user-agent updater
	useragent.Initialize()

	// Initialize auth system
	if err := auth.Initialize(); err != nil {
		log.Fatalf("[FATAL] Failed to initialize auth: %v", err)
	}
	log.Println("[INFO] Auth system initialized successfully")

	// Build HTTP mux
	mux := http.NewServeMux()

	// Root endpoint
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			handleNotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"name":        "droid2api",
			"version":     "2.0.0",
			"description": "OpenAI Compatible API Proxy (Go)",
			"endpoints": []string{
				"GET /v1/models",
				"POST /v1/chat/completions",
				"POST /v1/responses",
				"POST /v1/messages",
				"POST /v1/messages/count_tokens",
				"POST /v1/generate",
			},
		})
	})

	// API routes
	mux.HandleFunc("/v1/models", methodGuard("GET", handler.HandleModels))
	mux.HandleFunc("/v1/chat/completions", methodGuard("POST", handler.HandleChatCompletions))
	mux.HandleFunc("/v1/responses", methodGuard("POST", handler.HandleDirectResponses))
	mux.HandleFunc("/v1/messages/count_tokens", methodGuard("POST", handler.HandleCountTokens))
	mux.HandleFunc("/v1/messages", methodGuard("POST", handler.HandleDirectMessages))
	mux.HandleFunc("/v1/generate", methodGuard("POST", handler.HandleDirectGenerate))

	// Admin Web UI
	mux.HandleFunc("/admin", handler.HandleAdminPage)
	mux.HandleFunc("/admin/api/slots", handler.HandleAdminAPISlots)
	mux.HandleFunc("/admin/api/slots/", handler.HandleAdminAPISlotAction)

	// Wrap with CORS + recovery middleware
	wrapped := corsMiddleware(recoveryMiddleware(mux))

	addr := fmt.Sprintf(":%d", c.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           wrapped,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	log.Printf("[INFO] Starting server on http://localhost%s", addr)
	log.Println("[INFO] Available endpoints:")
	log.Println("[INFO]   GET  /v1/models")
	log.Println("[INFO]   POST /v1/chat/completions")
	log.Println("[INFO]   POST /v1/responses")
	log.Println("[INFO]   POST /v1/messages")
	log.Println("[INFO]   POST /v1/messages/count_tokens")
	log.Println("[INFO]   POST /v1/generate")
	log.Printf("[INFO]   Admin UI: http://localhost%s/admin", addr)

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("[FATAL] Server failed: %v", err)
	}
}

// corsMiddleware adds CORS headers.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, anthropic-version, X-Session-Id, X-Assistant-Message-Id")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoveryMiddleware catches panics.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[ERROR] Panic recovered: %v", err)
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// methodGuard restricts a handler to a specific HTTP method.
func methodGuard(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != method {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Method %s not allowed, use %s", r.Method, method),
			})
			return
		}
		h(w, r)
	}
}

// handleNotFound logs and responds to unmatched routes.
func handleNotFound(w http.ResponseWriter, r *http.Request) {
	log.Printf("[ERROR] Invalid request: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   "Not Found",
		"message": fmt.Sprintf("Path %s %s does not exist", r.Method, r.URL.Path),
		"availableEndpoints": []string{
			"GET /v1/models",
			"POST /v1/chat/completions",
			"POST /v1/responses",
			"POST /v1/messages",
			"POST /v1/messages/count_tokens",
			"POST /v1/generate",
		},
	})
}
