// Package rpc implements the JSON-RPC HTTP server for the Clique consensus
// client, exposing Ethereum Beacon Node-compatible endpoints for node
// monitoring and Clique-specific endpoints for chain inspection.
package rpc

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
)

// --- Dependency interfaces ---

// P2PNode is the subset of p2p.Host queried by the RPC server.
type P2PNode interface {
	PeerID() peer.ID
	Addrs() []ma.Multiaddr
	PeerCount() int
	ConnectedPeers() []peer.AddrInfo
}

// ChainState is the subset of forkchoice.Store queried by the RPC server.
type ChainState interface {
	Head() *types.Header
	GetByNumber(number uint64) (*types.Header, bool)
	TD(hash common.Hash) (*big.Int, bool)
}

// SnapshotProvider returns the consensus signer-set snapshot at the current head.
// Returns nil if the node has not yet processed any blocks.
type SnapshotProvider func() consensus.Snapshot

// --- JSON response/request types ---

// apiResponse wraps a success payload in the standard {"data": ...} envelope
// used by the Ethereum Beacon Node API.
type apiResponse struct {
	Data interface{} `json:"data"`
}

// apiError is returned as the response body for all error status codes.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NodeIdentity is the response body for GET /eth/v1/node/identity.
type NodeIdentity struct {
	PeerID       string   `json:"peer_id"`
	ENR          string   `json:"enr"`
	P2PAddresses []string `json:"p2p_addresses"`
}

// PeerInfo describes a single connected peer.
type PeerInfo struct {
	PeerID    string `json:"peer_id"`
	Address   string `json:"address"`
	Direction string `json:"direction"`
	State     string `json:"state"`
}

// PeersResponse is the response body for GET /eth/v1/node/peers.
type PeersResponse struct {
	Data []PeerInfo     `json:"data"`
	Meta map[string]int `json:"meta"`
}

// SyncStatus is the response body for GET /eth/v1/node/syncing.
type SyncStatus struct {
	// HeadSlot maps to the current head block number for Clique networks.
	HeadSlot     string `json:"head_slot"`
	SyncDistance string `json:"sync_distance"`
	IsSyncing    bool   `json:"is_syncing"`
}

// HeadInfo is the response body for GET /clique/v1/head.
type HeadInfo struct {
	Number          string `json:"number"`
	Hash            string `json:"hash"`
	Signer          string `json:"signer"`
	Timestamp       string `json:"timestamp"`
	TotalDifficulty string `json:"total_difficulty"`
}

// ValidatorsInfo is the response body for GET /clique/v1/validators.
type ValidatorsInfo struct {
	Signers []string `json:"signers"`
	Count   int      `json:"count"`
}

// BlockInfo is the response body for GET /clique/v1/blocks/{number}.
type BlockInfo struct {
	Number     string `json:"number"`
	Hash       string `json:"hash"`
	ParentHash string `json:"parent_hash"`
	Timestamp  string `json:"timestamp"`
	Difficulty string `json:"difficulty"`
	Signer     string `json:"signer"`
	Extra      string `json:"extra"`
}

// VoteInfo describes a single pending vote.
type VoteInfo struct {
	Signer    string `json:"signer"`
	Address   string `json:"address"`
	Authorize bool   `json:"authorize"`
	Block     string `json:"block"`
}

// VoteRequest is the request body for POST /clique/v1/vote.
type VoteRequest struct {
	// Address is the hex-encoded Ethereum address to vote for (0x-prefixed).
	Address string `json:"address"`
	// Authorize is true to add the signer, false to remove.
	Authorize bool `json:"authorize"`
}

// PendingVote holds the next vote to include in a produced block, set by
// POST /clique/v1/vote and consumed by the block producer (Phase 7).
type PendingVote struct {
	Address   common.Address
	Authorize bool
}
