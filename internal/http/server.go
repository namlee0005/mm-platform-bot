package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"mm-platform-engine/internal/exchange"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server represents the HTTP server for health checks and metrics
type Server struct {
	port      int
	server    *http.Server
	exchange  exchange.Exchange
	startTime time.Time
}

// NewServer creates a new HTTP server
func NewServer(port int, exchange exchange.Exchange) *Server {
	return &Server{
		port:     port,
		exchange: exchange,
	}
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/metrics", s.handleMetrics)          // Prometheus format
	mux.HandleFunc("/metrics-json", s.handleMetricsJSON) // JSON format (legacy)
	mux.HandleFunc("/orders", s.handleOrders)

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	s.startTime = time.Now()
	log.Printf("Starting HTTP server on port %d", s.port)

	// Start server in a goroutine
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()
	return nil
}

// Stop gracefully stops the HTTP server
func (s *Server) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}

	log.Println("Stopping HTTP server...")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := s.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %w", err)
	}

	log.Println("HTTP server stopped")
	return nil
}

// handleHealth handles health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	}); err != nil {
		log.Printf("Error encoding health response: %v", err)
	}
}

// handleReady handles readiness check requests
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// Check if exchange is ready
	// This is a simple check - could be more sophisticated
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"status": "ready",
		"time":   time.Now().Format(time.RFC3339),
	}); err != nil {
		log.Printf("Error encoding ready response: %v", err)
	}
}

// handleMetrics handles Prometheus metrics requests
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Serve Prometheus metrics
	promhttp.Handler().ServeHTTP(w, r)
}

// handleMetricsJSON handles JSON metrics requests (legacy endpoint)
func (s *Server) handleMetricsJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"timestamp": time.Now().Unix(),
		"uptime":    time.Since(s.startTime).Seconds(),
	}); err != nil {
		log.Printf("Error encoding metrics response: %v", err)
	}
}

// handleOrders handles requests to view open orders
func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		http.Error(w, "symbol parameter required", http.StatusBadRequest)
		return
	}

	orders, err := s.exchange.GetOpenOrders(r.Context(), symbol)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get orders: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol,
		"count":  len(orders),
		"orders": orders,
	}); err != nil {
		log.Printf("Error encoding orders response: %v", err)
	}
}
