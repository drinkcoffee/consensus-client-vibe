package rpc

import (
	"fmt"
	"net/http"
)

// handleNodeIdentity serves GET /eth/v1/node/identity.
// Returns the local peer's libp2p ID and listen addresses.
func (s *Server) handleNodeIdentity(w http.ResponseWriter, r *http.Request) {
	addrs := s.p2p.Addrs()
	addrStrings := make([]string, len(addrs))
	for i, a := range addrs {
		addrStrings[i] = fmt.Sprintf("%s/p2p/%s", a.String(), s.p2p.PeerID().String())
	}

	ok(w, NodeIdentity{
		PeerID:       s.p2p.PeerID().String(),
		ENR:          "", // ENR requires discv5 — not yet implemented
		P2PAddresses: addrStrings,
	})
}

// handleNodePeers serves GET /eth/v1/node/peers.
// Returns all currently connected peers.
func (s *Server) handleNodePeers(w http.ResponseWriter, r *http.Request) {
	connected := s.p2p.ConnectedPeers()
	peers := make([]PeerInfo, 0, len(connected))
	for _, ai := range connected {
		addr := ""
		if len(ai.Addrs) > 0 {
			addr = fmt.Sprintf("%s/p2p/%s", ai.Addrs[0].String(), ai.ID.String())
		}
		peers = append(peers, PeerInfo{
			PeerID:    ai.ID.String(),
			Address:   addr,
			Direction: "unknown", // direction requires per-connection tracking
			State:     "connected",
		})
	}

	writeJSON(w, http.StatusOK, PeersResponse{
		Data: peers,
		Meta: map[string]int{"count": len(peers)},
	})
}

// handleNodeHealth serves GET /eth/v1/node/health.
//
// Response codes:
//
//	200 — node is ready and not syncing
//	206 — node is syncing
//	503 — node has no peers (not yet ready)
func (s *Server) handleNodeHealth(w http.ResponseWriter, _ *http.Request) {
	head := s.chain.Head()
	if head == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if s.p2p.PeerCount() == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// Without a sync target we conservatively report "synced".
	w.WriteHeader(http.StatusOK)
}

// handleNodeSyncing serves GET /eth/v1/node/syncing.
func (s *Server) handleNodeSyncing(w http.ResponseWriter, r *http.Request) {
	head := s.chain.Head()
	headNum := uint64(0)
	if head != nil && head.Number != nil {
		headNum = head.Number.Uint64()
	}

	// Without an external sync target we report sync_distance=0.
	ok(w, SyncStatus{
		HeadSlot:     fmt.Sprintf("%d", headNum),
		SyncDistance: "0",
		IsSyncing:    false,
	})
}
