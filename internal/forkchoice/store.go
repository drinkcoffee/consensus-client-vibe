// Package forkchoice implements the Clique fork choice rule: the canonical
// chain is the one with the highest cumulative difficulty (heaviest chain),
// matching Ethereum's pre-Merge proof-of-work chain selection.
//
// The Store holds all known block headers, maintains a canonical-chain index
// (block number → hash), and tracks three chain tip pointers used by the
// Engine API's engine_forkchoiceUpdated call:
//
//   - Head      — the tip of the heaviest known chain
//   - Safe      — the most recent epoch-boundary block on the canonical chain
//                 (a Clique checkpoint; signer list is canonical here)
//   - Finalized — the epoch-boundary block two epochs before Head
//                 (treated as effectively irreversible for Clique networks)
//
// The Store does not verify headers; callers must run clique.Engine.VerifyHeader
// before calling AddBlock.
package forkchoice

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
	"github.com/rs/zerolog"
)

// ErrUnknownParent is returned by AddBlock when the parent of the submitted
// header is not yet in the store.
var ErrUnknownParent = errors.New("unknown parent block")

// blockEntry is the internal representation of a block in the store.
type blockEntry struct {
	Header *types.Header
	// TD is the total difficulty of the chain up to and including this block.
	TD *big.Int
}

// Store is the fork choice store. It is safe for concurrent use.
type Store struct {
	mu sync.RWMutex

	// blocks holds every known block, keyed by its CL header hash.
	blocks map[common.Hash]*blockEntry
	// numbers maps canonical-chain block numbers to their CL header hashes.
	numbers map[uint64]common.Hash

	// elHashes maps each CL header hash to the corresponding EL execution
	// payload hash. The two hashes diverge because the CL header carries the
	// full 97-byte Clique Extra (seal included), whereas the EL block only
	// has a 32-byte extraData field. The EL hash is what must be supplied to
	// engine_forkchoiceUpdated, while the CL hash is used internally for
	// snapshot computation and parent lookups.
	elHashes map[common.Hash]common.Hash

	head      *blockEntry // tip of the canonical (heaviest) chain
	safe      *blockEntry // latest epoch-boundary block on the canonical chain
	finalized *blockEntry // epoch-boundary block two epochs before head

	genesis common.Hash // CL hash of the genesis block
	epoch   uint64      // blocks per epoch (from Clique genesis config)

	log zerolog.Logger
}

// New creates a Store initialised with the given genesis block. epoch must
// match genesis.config.clique.epoch. Genesis becomes the initial head, safe,
// and finalized block.
func New(genesis *types.Header, epoch uint64) *Store {
	td := genesis.Difficulty
	if td == nil || td.Sign() <= 0 {
		td = big.NewInt(1)
	} else {
		td = new(big.Int).Set(td)
	}

	hash := genesis.Hash()
	entry := &blockEntry{Header: genesis, TD: td}

	s := &Store{
		blocks:    map[common.Hash]*blockEntry{hash: entry},
		numbers:   map[uint64]common.Hash{0: hash},
		elHashes:  map[common.Hash]common.Hash{hash: hash}, // genesis CL hash == EL hash
		head:      entry,
		safe:      entry,
		finalized: entry,
		genesis:   hash,
		epoch:     epoch,
		log:       log.With("forkchoice"),
	}
	return s
}

// AddBlock stores header and updates the canonical head if it extends the
// heaviest chain. Returns true when the canonical head changes.
//
// elHash is the execution payload hash for the corresponding EL block. It is
// stored alongside the CL header hash so that ForkchoiceState can return the
// correct EL hashes to engine_forkchoiceUpdated. For the genesis block these
// two hashes are identical; for all subsequent blocks they differ because the
// CL header carries the full 97-byte Clique Extra while the EL block only has
// a 32-byte extraData field.
//
// Returns ErrUnknownParent if header's parent has not been added yet.
// Returns nil error and false if the block was already known.
func (s *Store) AddBlock(header *types.Header, elHash common.Hash) (bool, error) {
	if header.Number == nil || header.Difficulty == nil {
		return false, fmt.Errorf("header has nil Number or Difficulty field")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	hash := header.Hash()

	if _, exists := s.blocks[hash]; exists {
		return false, nil // already known, no-op
	}

	parent, ok := s.blocks[header.ParentHash]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrUnknownParent, header.ParentHash)
	}

	td := new(big.Int).Add(parent.TD, header.Difficulty)
	entry := &blockEntry{Header: header, TD: td}
	s.blocks[hash] = entry
	s.elHashes[hash] = elHash

	if s.head == nil || td.Cmp(s.head.TD) > 0 {
		s.setHead(entry)
		return true, nil
	}

	s.log.Debug().
		Str("hash", hash.Hex()).
		Uint64("number", header.Number.Uint64()).
		Str("td", td.String()).
		Str("head_td", s.head.TD.String()).
		Msg("side-chain block stored (not head)")

	return false, nil
}

// Head returns the header at the tip of the canonical chain.
func (s *Store) Head() *types.Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.head == nil {
		return nil
	}
	return s.head.Header
}

// Safe returns the most recent epoch-boundary block on the canonical chain.
func (s *Store) Safe() *types.Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.safe == nil {
		return nil
	}
	return s.safe.Header
}

// Finalized returns the epoch-boundary block two epochs before the current head.
func (s *Store) Finalized() *types.Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.finalized == nil {
		return nil
	}
	return s.finalized.Header
}

// ForkchoiceState returns the engine.ForkchoiceStateV1 describing the current
// canonical tip, ready to be passed to engine_forkchoiceUpdated.
//
// All three hashes are EL execution payload hashes, not CL header hashes.
// The Engine API requires EL hashes because the execution client only knows
// about its own block tree; it has no visibility into the CL header format.
func (s *Store) ForkchoiceState() engine.ForkchoiceStateV1 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state := engine.ForkchoiceStateV1{}
	if s.head != nil {
		if elHash, ok := s.elHashes[s.head.Header.Hash()]; ok {
			state.HeadBlockHash = elHash
		}
	}
	if s.safe != nil {
		if elHash, ok := s.elHashes[s.safe.Header.Hash()]; ok {
			state.SafeBlockHash = elHash
		}
	}
	if s.finalized != nil {
		if elHash, ok := s.elHashes[s.finalized.Header.Hash()]; ok {
			state.FinalizedBlockHash = elHash
		}
	}
	return state
}

// GetByHash returns the header for the given block hash, if known.
func (s *Store) GetByHash(hash common.Hash) (*types.Header, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.blocks[hash]
	if !ok {
		return nil, false
	}
	return entry.Header, true
}

// GetByNumber returns the canonical-chain header at the given block number, if known.
func (s *Store) GetByNumber(number uint64) (*types.Header, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hash, ok := s.numbers[number]
	if !ok {
		return nil, false
	}
	entry, ok := s.blocks[hash]
	if !ok {
		return nil, false
	}
	return entry.Header, true
}

// TD returns the total difficulty of the chain ending at hash, if known.
// The returned value is a copy and safe to modify.
func (s *Store) TD(hash common.Hash) (*big.Int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.blocks[hash]
	if !ok {
		return nil, false
	}
	return new(big.Int).Set(entry.TD), true
}

// HasBlock reports whether the block with the given hash is in the store.
func (s *Store) HasBlock(hash common.Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.blocks[hash]
	return ok
}

// Len returns the total number of blocks held in the store.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blocks)
}

// --- internal ---

// setHead switches the canonical chain to terminate at newHead, performing a
// reorg if newHead is not a direct extension of the current head.
// Must be called with s.mu held for writing.
func (s *Store) setHead(newHead *blockEntry) {
	oldHead := s.head
	newHash := newHead.Header.Hash()
	newNum := newHead.Header.Number.Uint64()

	if oldHead == nil || newHead.Header.ParentHash == oldHead.Header.Hash() {
		// Direct extension — no reorg required.
		s.numbers[newNum] = newHash
		s.head = newHead
		s.updateSafeFinalized()

		s.log.Debug().
			Str("hash", newHash.Hex()).
			Uint64("number", newNum).
			Str("td", newHead.TD.String()).
			Msg("canonical head advanced")
		return
	}

	// Reorg: find the common ancestor and reconcile the canonical-chain index.
	ancestor := s.findCommonAncestor(oldHead, newHead)

	ancestorHash := common.Hash{}
	ancestorNum := uint64(0)
	if ancestor != nil {
		ancestorHash = ancestor.Header.Hash()
		ancestorNum = ancestor.Header.Number.Uint64()
	}

	// Remove old chain from the canonical index down to (not including) ancestor.
	for cur := oldHead; cur != nil && cur.Header.Hash() != ancestorHash; {
		delete(s.numbers, cur.Header.Number.Uint64())
		par, ok := s.blocks[cur.Header.ParentHash]
		if !ok {
			break
		}
		cur = par
	}

	// Collect new chain blocks from newHead back to (not including) ancestor.
	var toCanonise []*blockEntry
	for cur := newHead; cur != nil && cur.Header.Hash() != ancestorHash; {
		toCanonise = append(toCanonise, cur)
		par, ok := s.blocks[cur.Header.ParentHash]
		if !ok {
			break
		}
		cur = par
	}
	// Add them to the canonical index (order doesn't matter for a map).
	for _, e := range toCanonise {
		s.numbers[e.Header.Number.Uint64()] = e.Header.Hash()
	}

	s.head = newHead
	s.updateSafeFinalized()

	s.log.Info().
		Str("new_head", newHash.Hex()).
		Uint64("new_num", newNum).
		Str("old_head", oldHead.Header.Hash().Hex()).
		Uint64("old_num", oldHead.Header.Number.Uint64()).
		Uint64("common_ancestor", ancestorNum).
		Uint64("reorg_depth", newNum-ancestorNum).
		Msg("chain reorg")
}

// findCommonAncestor returns the most recent block that is an ancestor of both
// a and b. Returns nil if a common ancestor cannot be found (e.g. missing
// parent in the store).
// Must be called with s.mu held (at least for reading).
func (s *Store) findCommonAncestor(a, b *blockEntry) *blockEntry {
	// Equalise heights.
	for a.Header.Number.Uint64() > b.Header.Number.Uint64() {
		par, ok := s.blocks[a.Header.ParentHash]
		if !ok {
			return nil
		}
		a = par
	}
	for b.Header.Number.Uint64() > a.Header.Number.Uint64() {
		par, ok := s.blocks[b.Header.ParentHash]
		if !ok {
			return nil
		}
		b = par
	}
	// Walk both chains back until they share a hash.
	for a.Header.Hash() != b.Header.Hash() {
		parA, okA := s.blocks[a.Header.ParentHash]
		parB, okB := s.blocks[b.Header.ParentHash]
		if !okA || !okB {
			return nil
		}
		a = parA
		b = parB
	}
	return a
}

// updateSafeFinalized recomputes the safe and finalized pointers based on the
// current head and the epoch configuration.
// Must be called with s.mu held for writing.
func (s *Store) updateSafeFinalized() {
	if s.head == nil || s.epoch == 0 {
		return
	}

	genesis := s.blocks[s.genesis]
	headNum := s.head.Header.Number.Uint64()

	// Safe = latest epoch-boundary block on the canonical chain.
	safeNum := (headNum / s.epoch) * s.epoch
	s.safe = genesis // default: fall back to genesis
	if hash, ok := s.numbers[safeNum]; ok {
		if entry, ok := s.blocks[hash]; ok {
			s.safe = entry
		}
	}

	// Finalized = epoch-boundary block two epochs before the current safe.
	s.finalized = genesis // default: fall back to genesis
	epochIdx := headNum / s.epoch
	if epochIdx >= 2 {
		finalNum := (epochIdx - 2) * s.epoch
		if hash, ok := s.numbers[finalNum]; ok {
			if entry, ok := s.blocks[hash]; ok {
				s.finalized = entry
			}
		}
	}
}
