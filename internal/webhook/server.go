package webhook

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// AlertHandler is called when a valid alert is received.
// It should trigger diagnosis and send results to Telegram.
type AlertHandler func(ctx context.Context, targets []AlertTarget, payload *AlertmanagerPayload)

// Server is a lightweight HTTP server for receiving webhooks.
type Server struct {
	addr         string
	bearerToken  string
	alertHandler AlertHandler
	logger       *slog.Logger
	httpServer   *http.Server
}

// NewServer creates a new webhook server.
func NewServer(addr string, bearerToken string, handler AlertHandler, logger *slog.Logger) *Server {
	return &Server{
		addr:         addr,
		bearerToken:  bearerToken,
		alertHandler: handler,
		logger:       logger,
	}
}

// Run starts the HTTP server and blocks until context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	s.httpServer = &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	s.logger.Info("webhook server starting", "addr", s.addr)

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Auth check (optional)
	if s.bearerToken != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.bearerToken {
			s.logger.Warn("unauthorized webhook request", "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	payload, err := ParseAlertmanagerPayload(body)
	if err != nil {
		s.logger.Error("failed to parse alertmanager payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	targets := ExtractTargets(payload)

	s.logger.Info("alertmanager webhook received",
		"status", payload.Status,
		"alerts", len(payload.Alerts),
		"targets", len(targets),
	)

	if len(targets) > 0 && s.alertHandler != nil {
		go s.alertHandler(r.Context(), targets, payload)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}
