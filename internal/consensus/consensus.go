// Package consensus defines the abstract interfaces for pluggable consensus
// mechanisms. The only implementation available at present is Clique (EIP-225).
package consensus

import (
	"crypto/ecdsa"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// VoteRecord describes a single pending vote cast by an authorized signer.
type VoteRecord struct {
	Signer    common.Address
	Address   common.Address
	Authorize bool
	Block     uint64
}

// Snapshot represents the signer-authorization state at a specific block.
type Snapshot interface {
	// BlockNumber returns the block number at which this snapshot was taken.
	BlockNumber() uint64
	// BlockHash returns the block hash at which this snapshot was taken.
	BlockHash() common.Hash
	// IsAuthorized reports whether addr is an authorized signer.
	IsAuthorized(addr common.Address) bool
	// HasRecentlySigned reports whether signer has signed too recently to sign
	// the block at number.
	HasRecentlySigned(number uint64, signer common.Address) bool
	// SignerList returns the authorized signers sorted lexicographically.
	SignerList() []common.Address
	// InTurn reports whether signer is the designated in-turn signer for number.
	InTurn(number uint64, signer common.Address) bool
	// PendingVotes returns all pending votes in the current epoch.
	PendingVotes() []VoteRecord
}

// Engine is the interface through which the node interacts with the consensus
// mechanism. Currently the only implementation is Clique (EIP-225).
type Engine interface {
	// Period returns the minimum time between blocks in seconds.
	Period() uint64
	// Epoch returns the number of blocks between vote checkpoints.
	Epoch() uint64

	// ExtraVanity is the fixed byte count of the vanity prefix in Extra.
	ExtraVanity() int
	// ExtraSeal is the fixed byte count of the ECDSA seal suffix in Extra.
	ExtraSeal() int
	// EmptyUncleHash is the required UncleHash for all PoA blocks.
	EmptyUncleHash() common.Hash
	// NonceAuth is the nonce encoding an "authorize" vote.
	NonceAuth() types.BlockNonce
	// NonceDrop is the nonce encoding a "remove" vote (also used as "no vote").
	NonceDrop() types.BlockNonce

	// NewGenesisSnapshot derives the initial snapshot from the genesis header.
	NewGenesisSnapshot(genesis *types.Header) (Snapshot, error)
	// NewCheckpointSnapshot derives a snapshot from an epoch checkpoint header.
	NewCheckpointSnapshot(header *types.Header) (Snapshot, error)

	// VerifyHeader checks that header is valid given the snapshot at the parent.
	VerifyHeader(snap Snapshot, header *types.Header, parent *types.Header) error
	// CalcDifficulty returns the expected difficulty for signer at number.
	CalcDifficulty(snap Snapshot, number uint64, signer common.Address) *big.Int
	// Apply creates a new snapshot by replaying headers onto snap. The headers
	// must be consecutive blocks starting immediately after snap.BlockNumber().
	Apply(snap Snapshot, headers []*types.Header) (Snapshot, error)

	// SealHeader signs header with key, writing the signature into Extra.
	SealHeader(header *types.Header, key *ecdsa.PrivateKey) error
	// SignerFromHeader recovers the signer address from a sealed header.
	SignerFromHeader(header *types.Header) (common.Address, error)

	// BuildExtra returns the CL-side Extra bytes for a new block at number.
	// The EL always receives only a 32-byte vanity field; each consensus engine
	// encodes its own metadata (signer list, committed seals, etc.) in its own
	// format for the CL header.
	BuildExtra(snap Snapshot, number uint64) []byte
}

// BFTEngine is an optional extension of Engine for protocols that require
// multi-validator agreement before a block is final. The node detects it via
// type assertion and replaces the timer-based production loop with a BFT
// message loop.
type BFTEngine interface {
	// Quorum is the minimum number of validator signatures to commit a block.
	// For QBFT: floor(2N/3) + 1.
	Quorum(validatorCount int) int

	// VerifyProposal validates a block received in the PROPOSAL phase.
	// Unlike VerifyHeader it does not check committed seals (they do not exist
	// yet). Called by the QBFT core when processing an incoming PROPOSAL.
	VerifyProposal(snap Snapshot, header *types.Header, parent *types.Header) error

	// CommitBlock injects the collected committed seals into header's Extra
	// field and returns the new final header. Called once 2f+1 COMMIT messages
	// have been collected.
	CommitBlock(header *types.Header, committedSeals [][]byte) (*types.Header, error)
}
