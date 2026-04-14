package node

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	p2phost "github.com/peterrobinson/consensus-client-vibe/internal/p2p"
)

// handleBlock is the entry point for blocks arriving over Gossipsub. It runs
// the full import pipeline:
//
//  1. Decode the Clique header from the wire message.
//  2. Retrieve the parent header from the store.
//  3. Compute the Clique snapshot at the parent.
//  4. Verify the header against consensus rules.
//  5. Add the header to the fork-choice store.
//  6. If the head changed, notify the execution client via engine_forkchoiceUpdated.
//  7. Update the local head snapshot.
//  8. Reschedule block production for the new head.
func (n *Node) handleBlock(ctx context.Context, from libp2ppeer.ID, blk *p2phost.CliqueBlock) {
	header, err := blk.DecodeHeader()
	if err != nil {
		n.log.Warn().Err(err).Msg("handleBlock: decode header failed")
		return
	}

	num := header.Number.Uint64()
	hash := header.Hash()

	n.log.Debug().
		Uint64("number", num).
		Str("hash", hash.Hex()).
		Msg("received block from P2P")

	// Drop gossip blocks while a sync session is actively rewriting the store
	// and snapshot state. The block will arrive again on the next gossip cycle
	// or be covered by the sync itself.
	if !n.syncMu.TryLock() {
		n.log.Debug().Uint64("number", num).Msg("handleBlock: sync in progress, dropping gossip block")
		return
	}
	n.syncMu.Unlock()

	// Step 2: Parent must be in the store.
	parent, ok := n.stor.GetByHash(header.ParentHash)
	if !ok {
		n.log.Warn().
			Uint64("number", num).
			Str("parent", header.ParentHash.Hex()).
			Msg("handleBlock: unknown parent, triggering sync")
		if n.p2p != nil {
			go n.syncWithPeer(from)
		}
		return
	}

	// Step 3: Snapshot at the parent block.
	snap, err := n.computeSnapshotAt(parent)
	if err != nil {
		n.log.Warn().Err(err).Uint64("number", num-1).Msg("handleBlock: compute parent snapshot failed")
		return
	}

	// Step 4: Clique consensus verification.
	if err := n.cliq.VerifyHeader(snap, header, parent); err != nil {
		n.log.Warn().Err(err).Uint64("number", num).Msg("handleBlock: header verification failed")
		return
	}

	// Step 5: Add to fork-choice store.
	// blk.ExecutionPayloadHash is the EL block hash so ForkchoiceState can
	// supply the correct EL hash to engine_forkchoiceUpdated.
	headChanged, err := n.stor.AddBlock(header, blk.ExecutionPayloadHash, blk.PayloadJSON)
	if err != nil {
		n.log.Error().Err(err).Uint64("number", num).Msg("handleBlock: AddBlock failed")
		return
	}

	if !headChanged {
		// Valid side-chain block stored but not the new head.
		return
	}

	// Step 6: Deliver the execution payload to the local EL, then update
	// fork choice. We must call engine_newPayloadV3 before
	// engine_forkchoiceUpdatedV3: in post-merge beacon mode, Geth does not
	// proactively fetch unknown blocks from devp2p peers when FCU references
	// an unknown hash — it expects the CL to deliver payloads directly.
	// Skipping this step would leave the EL without the parent block, causing
	// engine_forkchoiceUpdatedV3 to return SYNCING indefinitely when the next
	// signer tries to build on top of this block.
	payload, err := blk.DecodePayload()
	if err != nil {
		n.log.Error().Err(err).Uint64("number", num).Msg("handleBlock: decode payload failed")
		return
	}
	payloadStatus, err := n.eng.NewPayloadV3(ctx, *payload, nil, common.Hash{})
	if err != nil {
		n.log.Error().Err(err).Uint64("number", num).Msg("handleBlock: NewPayloadV3 failed")
		return
	}
	if payloadStatus.Status == engine.PayloadStatusInvalid {
		errMsg := "<nil>"
		if payloadStatus.ValidationError != nil {
			errMsg = *payloadStatus.ValidationError
		}
		n.log.Warn().
			Str("status", payloadStatus.Status).
			Str("error", errMsg).
			Uint64("number", num).
			Msg("handleBlock: EL rejected payload")
		return
	}

	state := n.stor.ForkchoiceState()
	result, err := n.eng.ForkchoiceUpdatedV3(ctx, state, nil)
	if err != nil {
		n.log.Error().Err(err).Uint64("number", num).Msg("handleBlock: ForkchoiceUpdatedV3 failed")
		return
	}
	if result.PayloadStatus.Status != engine.PayloadStatusValid &&
		result.PayloadStatus.Status != engine.PayloadStatusSyncing {
		n.log.Warn().
			Str("status", result.PayloadStatus.Status).
			Uint64("number", num).
			Msg("handleBlock: unexpected FCU status")
	}

	// Step 7: Advance the head snapshot.
	newSnap, err := n.computeSnapshot(header)
	if err != nil {
		n.log.Error().Err(err).Uint64("number", num).Msg("handleBlock: compute new snapshot failed")
		return
	}
	n.setHeadSnapshot(newSnap)

	// Update P2P status to reflect the new head.
	if n.p2p != nil {
		n.p2p.SetStatus(p2phost.StatusMsg{
			NetworkID:   n.cfg.Node.NetworkID,
			GenesisHash: n.genesisSnap.BlockHash(),
			HeadHash:    hash,
			HeadNumber:  num,
		})
	}

	n.log.Info().
		Uint64("number", num).
		Str("hash", hash.Hex()).
		Msg("new canonical head (from P2P)")

	// Step 8: Reschedule production for the new head.
	n.scheduleBlockProduction(ctx)
}

// computeSnapshotAt returns the consensus snapshot at the given block (inclusive).
// It is equivalent to computeSnapshot but accepts a header that is already
// in the store. Used to get the snapshot AT a block (not after it).
func (n *Node) computeSnapshotAt(header *types.Header) (consensus.Snapshot, error) {
	num := header.Number.Uint64()

	// Genesis snapshot is pre-computed.
	if num == 0 {
		return n.genesisSnap, nil
	}

	// Check if this is the current head — snapshot already computed.
	n.mu.RLock()
	snap := n.headSnap
	n.mu.RUnlock()
	if snap != nil && snap.BlockNumber() == num && snap.BlockHash() == header.Hash() {
		return snap, nil
	}

	// Fall back to computeSnapshot which starts from epoch boundary.
	return n.computeSnapshot(header)
}

// importPayload submits an execution payload to the EL and validates the
// response. It is used when this node produces a block (the EL does not
// yet have the payload from devp2p).
func (n *Node) importPayload(ctx context.Context, payload engine.ExecutionPayloadV3) error {
	status, err := n.eng.NewPayloadV3(ctx, payload, nil, common.Hash{})
	if err != nil {
		return fmt.Errorf("engine_newPayloadV3: %w", err)
	}
	switch status.Status {
	case engine.PayloadStatusValid:
		return nil
	case engine.PayloadStatusSyncing, engine.PayloadStatusAccepted:
		n.log.Warn().Str("status", status.Status).Msg("importPayload: EL still syncing")
		return nil
	default:
		errMsg := "<nil>"
		if status.ValidationError != nil {
			errMsg = *status.ValidationError
		}
		return fmt.Errorf("EL rejected payload: status=%s error=%s", status.Status, errMsg)
	}
}
