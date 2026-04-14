package rpc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
	clique "github.com/peterrobinson/consensus-client-vibe/internal/consensus/clique"
)

// handleCliqueHead serves GET /clique/v1/head.
// Returns details about the canonical chain tip.
func (s *Server) handleCliqueHead(w http.ResponseWriter, r *http.Request) {
	head := s.chain.Head()
	if head == nil {
		writeError(w, http.StatusServiceUnavailable, "no head block available")
		return
	}

	signer, err := clique.SignerFromHeader(head)
	if err != nil {
		signer = zeroAddress()
	}

	td, _ := s.chain.TD(head.Hash())
	tdStr := "0"
	if td != nil {
		tdStr = td.String()
	}

	ok(w, HeadInfo{
		Number:          fmt.Sprintf("%d", head.Number.Uint64()),
		Hash:            head.Hash().Hex(),
		Signer:          signer.Hex(),
		Timestamp:       fmt.Sprintf("%d", head.Time),
		TotalDifficulty: tdStr,
	})
}

// handleCliqueValidators serves GET /clique/v1/validators.
// Returns the current authorized signer set.
func (s *Server) handleCliqueValidators(w http.ResponseWriter, r *http.Request) {
	snap := s.headSnapshot()
	if snap == nil {
		ok(w, ValidatorsInfo{Signers: []string{}, Count: 0})
		return
	}

	list := snap.SignerList()
	signers := make([]string, len(list))
	for i, addr := range list {
		signers[i] = addr.Hex()
	}
	ok(w, ValidatorsInfo{Signers: signers, Count: len(signers)})
}

// handleCliqueBlock serves GET /clique/v1/blocks/{number}.
// Returns header metadata for the canonical block at the given block number.
func (s *Server) handleCliqueBlock(w http.ResponseWriter, r *http.Request) {
	numStr := chi.URLParam(r, "number")
	num, err := strconv.ParseUint(numStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid block number: %s", numStr))
		return
	}

	h, ok2 := s.chain.GetByNumber(num)
	if !ok2 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("block %d not found", num))
		return
	}

	signer, err := clique.SignerFromHeader(h)
	if err != nil {
		signer = zeroAddress()
	}

	ok(w, BlockInfo{
		Number:     fmt.Sprintf("%d", h.Number.Uint64()),
		Hash:       h.Hash().Hex(),
		ParentHash: h.ParentHash.Hex(),
		Timestamp:  fmt.Sprintf("%d", h.Time),
		Difficulty: h.Difficulty.String(),
		Signer:     signer.Hex(),
		Extra:      fmt.Sprintf("0x%x", h.Extra),
	})
}

// handleCliqueVotes serves GET /clique/v1/votes.
// Returns all pending votes in the current epoch.
func (s *Server) handleCliqueVotes(w http.ResponseWriter, r *http.Request) {
	snap := s.headSnapshot()
	if snap == nil {
		ok(w, []VoteInfo{})
		return
	}

	pending := snap.PendingVotes()
	votes := make([]VoteInfo, len(pending))
	for i, v := range pending {
		votes[i] = VoteInfo{
			Signer:    v.Signer.Hex(),
			Address:   v.Address.Hex(),
			Authorize: v.Authorize,
			Block:     fmt.Sprintf("%d", v.Block),
		}
	}
	ok(w, votes)
}

// handleCliqueVote serves POST /clique/v1/vote.
// Stores a vote intent that will be included in the next block the node
// produces (if it is the in-turn signer). Accepts a JSON body:
//
//	{"address": "0x...", "authorize": true}
func (s *Server) handleCliqueVote(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 512))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req VoteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if !isHexAddress(req.Address) {
		writeError(w, http.StatusBadRequest, "address must be a 0x-prefixed 20-byte hex string")
		return
	}

	addr := hexToAddress(req.Address)

	// Validate that the address is a non-zero Ethereum address.
	if addr == (zeroAddress()) && !nonZeroHexAddress(req.Address) {
		writeError(w, http.StatusBadRequest, "zero address is not a valid vote target")
		return
	}

	s.mu.Lock()
	s.pendingVote = &PendingVote{Address: addr, Authorize: req.Authorize}
	s.mu.Unlock()

	s.log.Info().
		Str("address", addr.Hex()).
		Bool("authorize", req.Authorize).
		Msg("pending vote set")

	ok(w, map[string]string{"status": "pending vote set"})
}

// --- helpers ---

// headSnapshot returns the consensus snapshot at the current head, or nil.
func (s *Server) headSnapshot() consensus.Snapshot {
	if s.snap == nil {
		return nil
	}
	return s.snap()
}
