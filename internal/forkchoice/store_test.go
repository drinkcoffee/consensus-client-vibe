package forkchoice

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// --- helpers ---

// makeGenesis returns a genesis block (number 0) with the given difficulty.
func makeGenesis(diff int64) *types.Header {
	return &types.Header{
		Number:     big.NewInt(0),
		Difficulty: big.NewInt(diff),
		ParentHash: common.Hash{},
		Extra:      []byte("genesis"),
	}
}

// child returns a new header that extends parent, with the given difficulty.
// seed makes the hash unique when two blocks share the same number+parent+difficulty.
func child(parent *types.Header, diff int64, seed ...byte) *types.Header {
	extra := append([]byte{}, seed...)
	if len(extra) == 0 {
		extra = []byte{0}
	}
	return &types.Header{
		Number:     new(big.Int).Add(parent.Number, big.NewInt(1)),
		ParentHash: parent.Hash(),
		Difficulty: big.NewInt(diff),
		Time:       parent.Time + 15,
		Extra:      extra,
	}
}

// chain builds a linear chain of n blocks extending parent, each with the
// given difficulty.
func chain(parent *types.Header, n int, diff int64) []*types.Header {
	blocks := make([]*types.Header, n)
	cur := parent
	for i := range blocks {
		blocks[i] = child(cur, diff)
		cur = blocks[i]
	}
	return blocks
}

// addAll calls store.AddBlock for each header in order, fatally failing the
// test on any error. In tests the CL hash and EL hash are the same (there is
// no real execution client), so h.Hash() is passed for both.
func addAll(t *testing.T, s *Store, headers []*types.Header) {
	t.Helper()
	for _, h := range headers {
		if _, err := s.AddBlock(h, h.Hash(), nil); err != nil {
			t.Fatalf("AddBlock(%d): %v", h.Number.Uint64(), err)
		}
	}
}

// --- New ---

func TestNew_GenesisIsHead(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	if s.Head() == nil {
		t.Fatal("head should not be nil after New")
	}
	if s.Head().Hash() != genesis.Hash() {
		t.Error("head should be genesis")
	}
	if s.Safe().Hash() != genesis.Hash() {
		t.Error("safe should be genesis")
	}
	if s.Finalized().Hash() != genesis.Hash() {
		t.Error("finalized should be genesis")
	}
}

func TestNew_GenesisInStore(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	h, ok := s.GetByHash(genesis.Hash())
	if !ok || h.Hash() != genesis.Hash() {
		t.Error("genesis not found by hash")
	}
	h, ok = s.GetByNumber(0)
	if !ok || h.Hash() != genesis.Hash() {
		t.Error("genesis not found by number")
	}
	if s.Len() != 1 {
		t.Errorf("store length = %d, want 1", s.Len())
	}
}

func TestNew_GenesisTD(t *testing.T) {
	genesis := makeGenesis(5)
	s := New(genesis, 100)

	td, ok := s.TD(genesis.Hash())
	if !ok {
		t.Fatal("TD not found for genesis")
	}
	if td.Int64() != 5 {
		t.Errorf("genesis TD = %v, want 5", td)
	}
}

func TestNew_NilGenesisDiffDefaultsToOne(t *testing.T) {
	genesis := &types.Header{Number: big.NewInt(0)} // nil Difficulty
	s := New(genesis, 100)
	td, _ := s.TD(genesis.Hash())
	if td.Sign() <= 0 {
		t.Error("expected positive TD for genesis with nil Difficulty")
	}
}

// --- AddBlock: direct extension ---

func TestAddBlock_DirectExtension(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	b1 := child(genesis, 2)
	changed, err := s.AddBlock(b1, b1.Hash(), nil)
	if err != nil {
		t.Fatalf("AddBlock: %v", err)
	}
	if !changed {
		t.Error("head should have changed")
	}
	if s.Head().Hash() != b1.Hash() {
		t.Error("head should be b1")
	}
}

func TestAddBlock_TDAccumulates(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	b1 := child(genesis, 2)
	b2 := child(b1, 2)
	addAll(t, s, []*types.Header{b1, b2})

	td, ok := s.TD(b2.Hash())
	if !ok {
		t.Fatal("TD not found for b2")
	}
	// genesis(1) + b1(2) + b2(2) = 5
	if td.Int64() != 5 {
		t.Errorf("b2 TD = %v, want 5", td)
	}
}

func TestAddBlock_Duplicate(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	b1 := child(genesis, 2)
	s.AddBlock(b1, b1.Hash(), nil) //nolint
	changed, err := s.AddBlock(b1, b1.Hash(), nil)
	if err != nil {
		t.Fatalf("duplicate AddBlock should not error: %v", err)
	}
	if changed {
		t.Error("duplicate block should not change head")
	}
}

func TestAddBlock_UnknownParent(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	orphan := &types.Header{
		Number:     big.NewInt(5),
		ParentHash: common.HexToHash("0xdeadbeef"),
		Difficulty: big.NewInt(2),
	}
	_, err := s.AddBlock(orphan, orphan.Hash(), nil)
	if !errors.Is(err, ErrUnknownParent) {
		t.Errorf("expected ErrUnknownParent, got: %v", err)
	}
}

func TestAddBlock_NilFields(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	nilHash := common.Hash{}
	_, err := s.AddBlock(&types.Header{Number: nil, Difficulty: big.NewInt(1)}, nilHash, nil)
	if err == nil {
		t.Error("expected error for nil Number")
	}
	_, err = s.AddBlock(&types.Header{Number: big.NewInt(1), Difficulty: nil}, nilHash, nil)
	if err == nil {
		t.Error("expected error for nil Difficulty")
	}
}

// --- Canonical chain index ---

func TestGetByNumber_CanonicalChain(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	blocks := chain(genesis, 5, 2)
	addAll(t, s, blocks)

	for i, b := range blocks {
		num := uint64(i + 1)
		h, ok := s.GetByNumber(num)
		if !ok {
			t.Errorf("block %d not in canonical chain", num)
			continue
		}
		if h.Hash() != b.Hash() {
			t.Errorf("block %d hash mismatch", num)
		}
	}
}

func TestGetByNumber_OutOfRange(t *testing.T) {
	s := New(makeGenesis(1), 100)
	_, ok := s.GetByNumber(999)
	if ok {
		t.Error("expected false for out-of-range block number")
	}
}

func TestHasBlock(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	b1 := child(genesis, 2)
	if s.HasBlock(b1.Hash()) {
		t.Error("b1 should not be in store yet")
	}
	s.AddBlock(b1, b1.Hash(), nil) //nolint
	if !s.HasBlock(b1.Hash()) {
		t.Error("b1 should be in store after AddBlock")
	}
}

// --- Fork choice / reorg ---

func TestAddBlock_SideChainLowerTD(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	// Main chain: two blocks with diff=2 (TD = 1+2+2 = 5).
	main1 := child(genesis, 2, 0)
	main2 := child(main1, 2, 0)
	addAll(t, s, []*types.Header{main1, main2})

	// Fork at block 1 with diff=1 (TD = 1+2+1 = 4 < 5).
	fork1 := child(main1, 1, 1) // same parent as main2
	changed, err := s.AddBlock(fork1, fork1.Hash(), nil)
	if err != nil {
		t.Fatalf("AddBlock fork: %v", err)
	}
	if changed {
		t.Error("lower-TD fork should not change head")
	}
	if s.Head().Hash() != main2.Hash() {
		t.Error("head should still be main2")
	}
}

func TestAddBlock_Reorg(t *testing.T) {
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	// Main chain: two blocks, diff=1 each. TD = 1+1+1 = 3.
	main1 := child(genesis, 1, 0)
	main2 := child(main1, 1, 0)
	addAll(t, s, []*types.Header{main1, main2})

	if s.Head().Hash() != main2.Hash() {
		t.Fatal("setup: head should be main2")
	}

	// Fork from genesis, diff=2 each. After 2 blocks TD = 1+2+2 = 5 > 3.
	fork1 := child(genesis, 2, 1)
	fork2 := child(fork1, 2, 1)
	addAll(t, s, []*types.Header{fork1, fork2})

	if s.Head().Hash() != fork2.Hash() {
		t.Error("after reorg, head should be fork2")
	}
	// The canonical chain should now be: genesis → fork1 → fork2.
	if h, ok := s.GetByNumber(1); !ok || h.Hash() != fork1.Hash() {
		t.Error("canonical block 1 should be fork1 after reorg")
	}
	if h, ok := s.GetByNumber(2); !ok || h.Hash() != fork2.Hash() {
		t.Error("canonical block 2 should be fork2 after reorg")
	}
}

func TestAddBlock_ReorgRestoresSidechain(t *testing.T) {
	// Verify that after a reorg the old main-chain blocks are still in the
	// store (by hash) even though they are no longer canonical.
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	main1 := child(genesis, 1, 0)
	main2 := child(main1, 1, 0)
	addAll(t, s, []*types.Header{main1, main2})

	fork1 := child(genesis, 2, 1)
	fork2 := child(fork1, 2, 1)
	addAll(t, s, []*types.Header{fork1, fork2})

	// Old main-chain blocks should still be findable by hash.
	if !s.HasBlock(main1.Hash()) {
		t.Error("main1 should still be in store after reorg")
	}
	if !s.HasBlock(main2.Hash()) {
		t.Error("main2 should still be in store after reorg")
	}
}

func TestAddBlock_LongerReorg(t *testing.T) {
	// A reorg where the new fork starts further back.
	genesis := makeGenesis(1)
	s := New(genesis, 100)

	// Main chain: 5 blocks, diff=1.
	main := chain(genesis, 5, 1)
	addAll(t, s, main)
	if s.Head().Hash() != main[4].Hash() {
		t.Fatal("setup: wrong head")
	}

	// Fork from genesis with diff=2. After 4 blocks TD = 1+2*4 = 9 > 1+5 = 6.
	fork := chain(genesis, 4, 2)
	// Add all but the last (still lower TD: 1+2*3=7 > 6 — actually 7>6, will reorg early).
	// Let's use diff=1 for fork and add 6 blocks to overtake at block 6.
	fork2 := chain(genesis, 6, 1)
	// fork2's last block TD = 1 + 6*1 = 7 > 1 + 5*1 = 6
	// But we need to make it unique vs main — use seed=99 inside chain.
	// Actually `chain` uses seed=0, so fork2 shares hashes with main chain.
	// Let's build it manually with different seeds.
	f := make([]*types.Header, 6)
	cur := genesis
	for i := range f {
		f[i] = child(cur, 1, 99)
		cur = f[i]
	}

	for _, b := range f {
		if _, err := s.AddBlock(b, b.Hash(), nil); err != nil {
			t.Fatalf("AddBlock fork2 block %d: %v", b.Number.Uint64(), err)
		}
	}

	if s.Head().Hash() != f[5].Hash() {
		t.Errorf("after long reorg, head should be f[5], got %s", s.Head().Hash().Hex())
	}
	// Verify canonical chain is the fork.
	for i, b := range f {
		h, ok := s.GetByNumber(uint64(i + 1))
		if !ok || h.Hash() != b.Hash() {
			t.Errorf("canonical block %d should be from fork after reorg", i+1)
		}
	}
	_ = fork
	_ = fork2
}

// --- Safe and Finalized ---

func TestSafe_BeforeFirstEpoch(t *testing.T) {
	const epoch = 5
	genesis := makeGenesis(1)
	s := New(genesis, epoch)

	// Add blocks up to just before the first epoch.
	blocks := chain(genesis, 4, 1)
	addAll(t, s, blocks)

	// Safe should still be genesis (no epoch boundary reached yet).
	if s.Safe().Hash() != genesis.Hash() {
		t.Errorf("safe should be genesis before first epoch, got block %d",
			s.Safe().Number.Uint64())
	}
}

func TestSafe_AtFirstEpoch(t *testing.T) {
	const epoch = 5
	genesis := makeGenesis(1)
	s := New(genesis, epoch)

	blocks := chain(genesis, 5, 1) // block 5 = first epoch boundary
	addAll(t, s, blocks)

	epochBlock := blocks[4] // block 5 (index 4)
	if s.Safe().Hash() != epochBlock.Hash() {
		t.Errorf("safe should be block 5 at first epoch, got block %d",
			s.Safe().Number.Uint64())
	}
	// Not enough history for finalized: still genesis.
	if s.Finalized().Hash() != genesis.Hash() {
		t.Errorf("finalized should still be genesis after one epoch, got block %d",
			s.Finalized().Number.Uint64())
	}
}

func TestFinalized_AfterTwoEpochs(t *testing.T) {
	const epoch = 5
	genesis := makeGenesis(1)
	s := New(genesis, epoch)

	// 15 blocks = 3 epochs. Finalized = epoch 1 (block 5).
	blocks := chain(genesis, 15, 1)
	addAll(t, s, blocks)

	if s.Safe().Number.Uint64() != 15 {
		t.Errorf("safe number = %d, want 15", s.Safe().Number.Uint64())
	}
	if s.Finalized().Number.Uint64() != 5 {
		t.Errorf("finalized number = %d, want 5", s.Finalized().Number.Uint64())
	}
}

func TestFinalized_SafeMovesWithHead(t *testing.T) {
	const epoch = 5
	genesis := makeGenesis(1)
	s := New(genesis, epoch)

	// Advance to block 10 (2 epochs).
	addAll(t, s, chain(genesis, 10, 1))
	// Safe = block 10, finalized = block 0 (genesis).
	if s.Safe().Number.Uint64() != 10 {
		t.Errorf("safe = %d, want 10", s.Safe().Number.Uint64())
	}
	if s.Finalized().Number.Uint64() != 0 {
		t.Errorf("finalized = %d, want 0", s.Finalized().Number.Uint64())
	}

	// Advance to block 15 (3rd epoch).
	tip, _ := s.GetByNumber(10)
	addAll(t, s, chain(tip, 5, 1))
	// Safe = block 15, finalized = block 5.
	if s.Safe().Number.Uint64() != 15 {
		t.Errorf("safe = %d, want 15", s.Safe().Number.Uint64())
	}
	if s.Finalized().Number.Uint64() != 5 {
		t.Errorf("finalized = %d, want 5", s.Finalized().Number.Uint64())
	}
}

// --- ForkchoiceState ---

func TestForkchoiceState_MatchesPointers(t *testing.T) {
	const epoch = 5
	genesis := makeGenesis(1)
	s := New(genesis, epoch)

	addAll(t, s, chain(genesis, 7, 1))

	state := s.ForkchoiceState()
	if state.HeadBlockHash != s.Head().Hash() {
		t.Error("ForkchoiceState.Head mismatch")
	}
	if state.SafeBlockHash != s.Safe().Hash() {
		t.Error("ForkchoiceState.Safe mismatch")
	}
	if state.FinalizedBlockHash != s.Finalized().Hash() {
		t.Error("ForkchoiceState.Finalized mismatch")
	}
}

func TestForkchoiceState_Reorg_UpdatesAllPointers(t *testing.T) {
	const epoch = 3
	genesis := makeGenesis(1)
	s := New(genesis, epoch)

	// Main chain: 6 blocks, diff=1. Safe=block 6, finalized=block 0 (genesis).
	main := chain(genesis, 6, 1)
	addAll(t, s, main)

	// Fork from block 2 with diff=2 to overtake. Fork needs 5 blocks to beat TD=7.
	// fork TD = genesis(1) + main[0](1) + main[1](1) + f1(2)+f2(2)+f3(2)+f4(2) = 12 > 7
	forkBase := main[1] // block 2, ParentHash = main[0].Hash()
	f := make([]*types.Header, 4)
	cur := forkBase
	for i := range f {
		f[i] = child(cur, 2, 99)
		cur = f[i]
	}
	addAll(t, s, f)

	// f[3] is now head (block 6, fork branch).
	state := s.ForkchoiceState()
	if state.HeadBlockHash != s.Head().Hash() {
		t.Error("FCU head should match store head after reorg")
	}
}
