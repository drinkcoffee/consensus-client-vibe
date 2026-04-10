package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
)

// Server is the JSON-RPC HTTP server.
type Server struct {
	p2p   P2PNode
	chain ChainState
	snap  SnapshotProvider // may be nil

	mu          sync.RWMutex
	pendingVote *PendingVote // set by POST /clique/v1/vote

	router *chi.Mux
	srv    *http.Server
	log    zerolog.Logger
}

// New creates a Server wired to the given subsystems. snap may be nil; in
// that case the validators and votes endpoints return empty data.
func New(cfg *config.RPCConfig, p2p P2PNode, chain ChainState, snap SnapshotProvider) *Server {
	s := &Server{
		p2p:   p2p,
		chain: chain,
		snap:  snap,
		log:   log.With("rpc"),
	}
	s.router = s.buildRouter()
	s.srv = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      s.router,
		ReadTimeout:  cfg.ReadTimeout.Duration,
		WriteTimeout: cfg.WriteTimeout.Duration,
	}
	return s
}

// Start begins listening for HTTP connections. It blocks until the server
// encounters an error (other than http.ErrServerClosed). Call Stop to
// initiate graceful shutdown.
func (s *Server) Start() error {
	s.log.Info().Str("addr", s.srv.Addr).Msg("RPC server listening")
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("RPC server: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the HTTP server, waiting for in-flight requests
// to complete or until ctx is cancelled.
func (s *Server) Stop(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// PendingVote returns the pending vote set by POST /clique/v1/vote, or nil if
// none has been set. Once returned, the pending vote is cleared.
func (s *Server) PendingVote() *PendingVote {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.pendingVote
	s.pendingVote = nil
	return v
}

// buildRouter constructs the chi router with all routes registered.
func (s *Server) buildRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.requestLogger)

	// Ethereum Beacon Node-compatible node endpoints.
	r.Get("/eth/v1/node/identity", s.handleNodeIdentity)
	r.Get("/eth/v1/node/peers", s.handleNodePeers)
	r.Get("/eth/v1/node/health", s.handleNodeHealth)
	r.Get("/eth/v1/node/syncing", s.handleNodeSyncing)

	// Clique-specific endpoints.
	r.Get("/clique/v1/head", s.handleCliqueHead)
	r.Get("/clique/v1/validators", s.handleCliqueValidators)
	r.Get("/clique/v1/blocks/{number}", s.handleCliqueBlock)
	r.Get("/clique/v1/votes", s.handleCliqueVotes)
	r.Post("/clique/v1/vote", s.handleCliqueVote)

	return r
}

// requestLogger is a chi middleware that logs each request at debug level.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Msg("RPC request")
		next.ServeHTTP(w, r)
	})
}

// --- helpers ---

// writeJSON serialises v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response with the given HTTP status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Code: status, Message: msg})
}

// ok writes a standard {"data": v} success response.
func ok(w http.ResponseWriter, v interface{}) {
	writeJSON(w, http.StatusOK, apiResponse{Data: v})
}
