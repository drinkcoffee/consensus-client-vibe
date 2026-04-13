package p2p

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// syncProtocol is the libp2p stream protocol ID for the catch-up sync exchange.
	syncProtocol = "/clique/sync/1"
	// MaxSyncBlocks is the maximum number of blocks returned per sync response.
	// Callers loop until they receive fewer than this many blocks.
	MaxSyncBlocks = 2048
	// maxSyncMsgSize caps the size of a sync response (2048 headers ≈ a few MB).
	maxSyncMsgSize = 32 * 1024 * 1024 // 32 MB
	syncTimeout    = 60 * time.Second
)

// SyncRequestMsg is sent by a node that needs to catch up to the canonical chain.
// HeadNumber and HeadHash identify the highest block the requester already has.
// For a node at genesis, HeadNumber=0 and HeadHash=genesisHash.
type SyncRequestMsg struct {
	HeadNumber uint64
	HeadHash   common.Hash
}

// SyncResponseMsg carries a batch of blocks in response to a SyncRequestMsg.
// If Reset is true the responder detected a chain divergence: the requester
// must discard all its stored headers and replay Blocks from block 1.
// Blocks are ordered from lowest to highest block number.
type SyncResponseMsg struct {
	Reset  bool
	Blocks []SyncBlock
}

// SyncBlock is a single CL header + EL payload hash + execution payload in a
// sync response. PayloadJSON is the JSON-encoded engine.ExecutionPayloadV3;
// receiving nodes must deliver it to their local EL via engine_newPayloadV3
// before sending engine_forkchoiceUpdated (post-merge Geth will not auto-fetch
// unknown blocks from devp2p peers in beacon mode).
type SyncBlock struct {
	Header      rlp.RawValue // RLP-encoded *types.Header
	ELHash      common.Hash
	PayloadJSON []byte // JSON-encoded ExecutionPayloadV3; nil for genesis
}

// SyncProvider is called to serve an incoming sync request. Implementations
// query the fork-choice store and return the appropriate response.
type SyncProvider func(req SyncRequestMsg) SyncResponseMsg

// SyncHandler is invoked (in a new goroutine) after a status exchange when the
// remote peer's head is ahead of ours. Implementations should open a sync
// stream to pid and import the missing blocks.
type SyncHandler func(pid peer.ID)

// SetSyncProvider registers the callback used to serve incoming /clique/sync/1
// streams. It also registers the stream handler on the libp2p host so that
// inbound sync requests are accepted.
func (h *Host) SetSyncProvider(fn SyncProvider) {
	h.mu.Lock()
	h.syncProvider = fn
	h.mu.Unlock()
	h.h.SetStreamHandler(syncProtocol, h.handleSyncStream)
}

// SetSyncHandler registers the callback invoked when a connected peer's head
// is higher than ours.
func (h *Host) SetSyncHandler(fn SyncHandler) {
	h.mu.Lock()
	h.syncHandler = fn
	h.mu.Unlock()
}

// RequestSync opens a /clique/sync/1 stream to pid, sends req, and returns
// the peer's response. The caller is responsible for looping until fully synced.
func (h *Host) RequestSync(ctx context.Context, pid peer.ID, req SyncRequestMsg) (SyncResponseMsg, error) {
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	s, err := h.h.NewStream(syncCtx, pid, syncProtocol)
	if err != nil {
		return SyncResponseMsg{}, fmt.Errorf("open sync stream to %s: %w", pid, err)
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(syncTimeout))

	if err := writeMsg(s, &req); err != nil {
		_ = s.Reset()
		return SyncResponseMsg{}, fmt.Errorf("send sync request: %w", err)
	}
	var resp SyncResponseMsg
	if err := readMsg(s, &resp, maxSyncMsgSize); err != nil {
		_ = s.Reset()
		return SyncResponseMsg{}, fmt.Errorf("read sync response: %w", err)
	}
	return resp, nil
}

// handleSyncStream is the server-side handler for inbound /clique/sync/1 streams.
func (h *Host) handleSyncStream(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(syncTimeout))
	pid := s.Conn().RemotePeer()

	var req SyncRequestMsg
	if err := readMsg(s, &req, maxStatusMsgSize); err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("sync: read request failed")
		_ = s.Reset()
		return
	}

	h.mu.RLock()
	provider := h.syncProvider
	h.mu.RUnlock()

	var resp SyncResponseMsg
	if provider != nil {
		resp = provider(req)
	}

	h.log.Debug().
		Str("peer", pid.String()).
		Uint64("req_head", req.HeadNumber).
		Int("blocks", len(resp.Blocks)).
		Bool("reset", resp.Reset).
		Msg("sync: serving response")

	if err := writeMsg(s, &resp); err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("sync: write response failed")
		_ = s.Reset()
	}
}
