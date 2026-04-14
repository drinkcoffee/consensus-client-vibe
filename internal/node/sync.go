package node

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	"github.com/peterrobinson/consensus-client-vibe/internal/forkchoice"
	p2phost "github.com/peterrobinson/consensus-client-vibe/internal/p2p"
)

// replayChain reads all records from db and replays them into the in-memory
// store. The DB is attached to the store only after the replay so that
// records already on disk are not written a second time. Called once at startup.
func (n *Node) replayChain(db *forkchoice.ChainDB) {
	records, err := db.ReadAll()
	if err != nil {
		n.log.Warn().Err(err).Msg("chain db: read failed, starting fresh")
		n.stor.SetDB(db)
		return
	}
	if len(records) == 0 {
		n.stor.SetDB(db)
		return
	}

	skipped := 0
	for _, rec := range records {
		if _, err := n.stor.AddBlock(rec.Header, rec.ELHash, rec.PayloadJSON); err != nil {
			n.log.Debug().Err(err).Uint64("number", rec.Header.Number.Uint64()).Msg("chain db: skipping block during replay")
			skipped++
		}
	}

	// Attach the DB now so future blocks are persisted without re-writing what
	// we just replayed.
	n.stor.SetDB(db)

	// Compute the head snapshot from the replayed chain in one pass.
	head := n.stor.Head()
	if head.Number.Uint64() > 0 {
		snap, err := n.computeSnapshot(head)
		if err != nil {
			n.log.Warn().Err(err).Msg("chain db: compute head snapshot failed after replay")
		} else {
			n.mu.Lock()
			n.headSnap = snap
			n.mu.Unlock()
		}
	}

	// Update P2P status to reflect the recovered head.
	if n.p2p != nil {
		n.p2p.SetStatus(p2phost.StatusMsg{
			NetworkID:   n.cfg.Node.NetworkID,
			GenesisHash: n.genesisSnap.BlockHash(),
			HeadHash:    head.Hash(),
			HeadNumber:  head.Number.Uint64(),
		})
	}

	n.log.Info().
		Uint64("head", head.Number.Uint64()).
		Str("hash", head.Hash().Hex()).
		Int("replayed", len(records)-skipped).
		Int("skipped", skipped).
		Msg("chain db: replay complete")
}

// syncWithPeer is the SyncHandler callback registered with the P2P host. It
// runs in its own goroutine whenever a peer's status shows a head higher than
// ours. It loops until the peer has no more blocks to give us, then sends a
// single engine_forkchoiceUpdatedV3 to align the EL with the new head.
//
// Only one sync runs at a time (guarded by n.syncMu.TryLock). Concurrent
// invocations for the same or different peers return immediately.
func (n *Node) syncWithPeer(pid libp2ppeer.ID) {
	if !n.syncMu.TryLock() {
		n.log.Debug().Str("peer", pid.String()).Msg("sync: already in progress, skipping")
		return
	}
	defer n.syncMu.Unlock()

	n.log.Info().Str("peer", pid.String()).Msg("sync: starting")

	ctx := n.runCtx
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		head := n.stor.Head()
		req := p2phost.SyncRequestMsg{
			HeadNumber: head.Number.Uint64(),
			HeadHash:   head.Hash(),
		}

		resp, err := n.p2p.RequestSync(ctx, pid, req)
		if err != nil {
			n.log.Warn().Err(err).Str("peer", pid.String()).Msg("sync: request failed")
			return
		}

		if len(resp.Blocks) == 0 {
			n.log.Debug().Str("peer", pid.String()).Msg("sync: already up to date")
			break
		}

		if resp.Reset {
			n.log.Info().
				Str("peer", pid.String()).
				Uint64("our_head", req.HeadNumber).
				Msg("sync: chain divergence detected, resetting to genesis")
			if err := n.resetChain(); err != nil {
				n.log.Error().Err(err).Msg("sync: reset failed")
				return
			}
		}

		n.log.Info().
			Str("peer", pid.String()).
			Int("blocks", len(resp.Blocks)).
			Msg("sync: importing blocks")

		if err := n.importSyncBlocks(ctx, resp.Blocks); err != nil {
			n.log.Error().Err(err).Str("peer", pid.String()).Msg("sync: import failed")
			return
		}

		if len(resp.Blocks) < p2phost.MaxSyncBlocks {
			break // received final (partial) batch — sync complete
		}
	}

	// Send one FCU to align the EL with the new CL head.
	head := n.stor.Head()
	state := n.stor.ForkchoiceState()
	if _, err := n.eng.ForkchoiceUpdatedV3(ctx, state, nil); err != nil {
		n.log.Warn().Err(err).Msg("sync: FCU after import failed")
	}

	if n.p2p != nil {
		n.p2p.SetStatus(p2phost.StatusMsg{
			NetworkID:   n.cfg.Node.NetworkID,
			GenesisHash: n.genesisSnap.BlockHash(),
			HeadHash:    head.Hash(),
			HeadNumber:  head.Number.Uint64(),
		})
	}

	n.log.Info().
		Uint64("head", head.Number.Uint64()).
		Str("hash", head.Hash().Hex()).
		Msg("sync: complete")

	n.scheduleBlockProduction(ctx)
}

// resetChain truncates the on-disk DB and resets the in-memory store and
// snapshot caches back to genesis. New blocks imported after the reset will be
// persisted to the (now-empty) DB.
func (n *Node) resetChain() error {
	if n.db != nil {
		if err := n.db.Truncate(); err != nil {
			return fmt.Errorf("truncate chain db: %w", err)
		}
	}
	n.stor.Reset(n.genesisHeader)
	n.mu.Lock()
	n.headSnap = n.genesisSnap
	n.epochSnaps = make(map[uint64]consensus.Snapshot)
	n.mu.Unlock()
	return nil
}

// importSyncBlocks validates and imports a batch of blocks received from the
// sync protocol. For each block it delivers the execution payload to the local
// EL via engine_newPayloadV3 so that subsequent FCU calls succeed. Post-merge
// Geth in beacon mode does not fetch unknown payloads from devp2p peers on its
// own — the CL must push them explicitly. A single FCU is sent after the entire
// sync loop completes (see syncWithPeer).
func (n *Node) importSyncBlocks(ctx context.Context, blocks []p2phost.SyncBlock) error {
	for i, sb := range blocks {
		var h types.Header
		if err := rlp.DecodeBytes(sb.Header, &h); err != nil {
			return fmt.Errorf("block %d: decode header: %w", i, err)
		}

		parent, ok := n.stor.GetByHash(h.ParentHash)
		if !ok {
			return fmt.Errorf("block %d (number %d): unknown parent %s", i, h.Number, h.ParentHash.Hex())
		}

		parentSnap, err := n.computeSnapshotAt(parent)
		if err != nil {
			return fmt.Errorf("block %d (number %d): compute parent snapshot: %w", i, h.Number, err)
		}

		if err := n.cliq.VerifyHeader(parentSnap, &h, parent); err != nil {
			return fmt.Errorf("block %d (number %d): verify header: %w", i, h.Number, err)
		}

		// Deliver the execution payload to the local EL before updating the
		// store. Without this, the subsequent FCU will return SYNCING.
		if len(sb.PayloadJSON) > 0 {
			var payload engine.ExecutionPayloadV3
			if err := json.Unmarshal(sb.PayloadJSON, &payload); err != nil {
				return fmt.Errorf("block %d (number %d): decode payload: %w", i, h.Number, err)
			}
			status, err := n.eng.NewPayloadV3(ctx, payload, nil, common.Hash{})
			if err != nil {
				return fmt.Errorf("block %d (number %d): engine_newPayloadV3: %w", i, h.Number, err)
			}
			if status.Status == engine.PayloadStatusInvalid {
				errMsg := "<nil>"
				if status.ValidationError != nil {
					errMsg = *status.ValidationError
				}
				return fmt.Errorf("block %d (number %d): EL rejected payload: %s", i, h.Number, errMsg)
			}
		}

		if _, err := n.stor.AddBlock(&h, sb.ELHash, sb.PayloadJSON); err != nil {
			return fmt.Errorf("block %d (number %d): add to store: %w", i, h.Number, err)
		}

		// Update head snapshot incrementally. computeSnapshot uses the fast
		// path (O(1) via cliq.Apply) when adding blocks one-by-one to the head.
		newSnap, err := n.computeSnapshot(&h)
		if err != nil {
			return fmt.Errorf("block %d (number %d): compute snapshot: %w", i, h.Number, err)
		}
		n.setHeadSnapshot(newSnap)
	}
	return nil
}

// handleSyncRequest is the SyncProvider callback registered with the P2P host.
// It is called when a peer opens a /clique/sync/1 stream to us. It compares
// the peer's head against our canonical chain and returns the blocks they need.
func (n *Node) handleSyncRequest(req p2phost.SyncRequestMsg) p2phost.SyncResponseMsg {
	ourHead := n.stor.Head()
	if ourHead == nil {
		return p2phost.SyncResponseMsg{}
	}

	var startNum uint64
	reset := false

	if req.HeadNumber == 0 {
		// Requester is at genesis — send from block 1.
		startNum = 1
	} else {
		canonical, ok := n.stor.GetByNumber(req.HeadNumber)
		if !ok || canonical.Hash() != req.HeadHash {
			// Requester's chain diverges from ours.
			reset = true
			startNum = 1
		} else {
			startNum = req.HeadNumber + 1
		}
	}

	endNum := ourHead.Number.Uint64()
	if endNum < startNum {
		return p2phost.SyncResponseMsg{Reset: reset}
	}
	if endNum-startNum+1 > p2phost.MaxSyncBlocks {
		endNum = startNum + p2phost.MaxSyncBlocks - 1
	}

	headers, elHashes := n.stor.BlocksInRange(startNum, endNum)
	blocks := make([]p2phost.SyncBlock, 0, len(headers))
	for i, h := range headers {
		raw, err := rlp.EncodeToBytes(h)
		if err != nil {
			n.log.Warn().Err(err).Uint64("number", h.Number.Uint64()).Msg("sync: encode header failed, truncating response")
			break
		}
		payloadJSON, _ := n.stor.GetPayload(h.Hash())
		blocks = append(blocks, p2phost.SyncBlock{Header: raw, ELHash: elHashes[i], PayloadJSON: payloadJSON})
	}

	return p2phost.SyncResponseMsg{Reset: reset, Blocks: blocks}
}
