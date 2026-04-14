package clique

import (
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
)

// Compile-time interface checks.
var _ consensus.Engine = (*Engine)(nil)
var _ consensus.Snapshot = (*Snapshot)(nil)

// --- consensus.Snapshot methods on *Snapshot ---

// BlockNumber returns the block number at which this snapshot was taken.
func (s *Snapshot) BlockNumber() uint64 { return s.Number }

// BlockHash returns the block hash at which this snapshot was taken.
func (s *Snapshot) BlockHash() common.Hash { return s.Hash }

// PendingVotes returns all pending votes in the current epoch as VoteRecords.
func (s *Snapshot) PendingVotes() []consensus.VoteRecord {
	records := make([]consensus.VoteRecord, len(s.Votes))
	for i, v := range s.Votes {
		records[i] = consensus.VoteRecord{
			Signer:    v.Signer,
			Address:   v.Address,
			Authorize: v.Authorize,
			Block:     v.Block,
		}
	}
	return records
}

// --- consensus.Engine methods on *Engine ---

// ExtraVanity returns the fixed byte count of the vanity prefix in Extra.
func (e *Engine) ExtraVanity() int { return ExtraVanity }

// ExtraSeal returns the fixed byte count of the ECDSA seal suffix in Extra.
func (e *Engine) ExtraSeal() int { return ExtraSeal }

// EmptyUncleHash returns the required UncleHash for all Clique blocks.
func (e *Engine) EmptyUncleHash() common.Hash { return emptyUncleHash }

// NonceAuth returns the BlockNonce value encoding an "authorize" vote.
func (e *Engine) NonceAuth() types.BlockNonce { return nonceAuth }

// NonceDrop returns the BlockNonce value encoding a "remove" vote.
func (e *Engine) NonceDrop() types.BlockNonce { return nonceNull }

// NewGenesisSnapshot derives the initial snapshot from the genesis header.
func (e *Engine) NewGenesisSnapshot(genesis *types.Header) (consensus.Snapshot, error) {
	return NewGenesisSnapshot(genesis)
}

// NewCheckpointSnapshot derives a snapshot from an epoch checkpoint header.
func (e *Engine) NewCheckpointSnapshot(header *types.Header) (consensus.Snapshot, error) {
	return NewCheckpointSnapshot(header)
}

// SealHeader signs header with key, writing the signature into Extra.
func (e *Engine) SealHeader(header *types.Header, key *ecdsa.PrivateKey) error {
	return SealHeader(header, key)
}

// SignerFromHeader recovers the signer address from a sealed header.
func (e *Engine) SignerFromHeader(header *types.Header) (common.Address, error) {
	return SignerFromHeader(header)
}

// BuildExtra constructs the Extra field for the next Clique block.
// At epoch boundaries, the signer list is embedded after the vanity bytes.
// The last ExtraSeal bytes are zero-padded (to be filled in by SealHeader).
func (e *Engine) BuildExtra(snap consensus.Snapshot, number uint64) []byte {
	extra := make([]byte, ExtraVanity)
	if number%e.epoch == 0 {
		for _, addr := range snap.SignerList() {
			extra = append(extra, addr.Bytes()...)
		}
	}
	extra = append(extra, make([]byte, ExtraSeal)...)
	return extra
}
