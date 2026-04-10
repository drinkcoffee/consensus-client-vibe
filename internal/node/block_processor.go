package node

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	cliqueeng "github.com/peterrobinson/consensus-client-vibe/internal/clique"
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
func (n *Node) handleBlock(ctx context.Context, blk *p2phost.CliqueBlock) {
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

	// Step 2: Parent must be in the store.
	parent, ok := n.stor.GetByHash(header.ParentHash)
	if !ok {
		n.log.Warn().
			Uint64("number", num).
			Str("parent", header.ParentHash.Hex()).
			Msg("handleBlock: unknown parent, dropping block")
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
	headChanged, err := n.stor.AddBlock(header)
	if err != nil {
		n.log.Error().Err(err).Uint64("number", num).Msg("handleBlock: AddBlock failed")
		return
	}

	if !headChanged {
		// Valid side-chain block stored but not the new head.
		return
	}

	// Step 6: Notify the execution client of the new canonical head.
	// We do not call engine_newPayloadV3 here because the EL is expected to
	// receive the execution payload via its own devp2p. We simply update the
	// fork-choice pointers.
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
			GenesisHash: n.genesisSnap.Hash,
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

// computeSnapshotAt returns the Clique snapshot at the given block (inclusive).
// It is equivalent to computeSnapshot but accepts a header that is already
// in the store. Used to get the snapshot AT a block (not after it).
func (n *Node) computeSnapshotAt(header *types.Header) (*cliqueeng.Snapshot, error) {
	num := header.Number.Uint64()

	// Genesis snapshot is pre-computed.
	if num == 0 {
		return n.genesisSnap, nil
	}

	// Check if this is the current head — snapshot already computed.
	n.mu.RLock()
	snap := n.headSnap
	n.mu.RUnlock()
	if snap != nil && snap.Number == num && snap.Hash == header.Hash() {
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
