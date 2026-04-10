package clique

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// --- NewGenesisSnapshot ---

func TestNewGenesisSnapshot_Basic(t *testing.T) {
	k1, k2, k3 := testKey(t), testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2), addr(k3)}

	snap, err := NewGenesisSnapshot(&types.Header{
		Number: big.NewInt(0),
		Extra:  genesisExtra(signers),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snap.Signers) != 3 {
		t.Errorf("want 3 signers, got %d", len(snap.Signers))
	}
	for _, s := range signers {
		if !snap.IsAuthorized(s) {
			t.Errorf("signer %s not found in snapshot", s)
		}
	}
	if snap.Number != 0 {
		t.Errorf("number = %d, want 0", snap.Number)
	}
}

func TestNewGenesisSnapshot_NoSigners(t *testing.T) {
	_, err := NewGenesisSnapshot(&types.Header{
		Number: big.NewInt(0),
		Extra:  make([]byte, extraVanity+extraSeal), // no signer bytes
	})
	if err == nil {
		t.Error("expected error for zero signers")
	}
}

func TestNewGenesisSnapshot_ShortExtra(t *testing.T) {
	_, err := NewGenesisSnapshot(&types.Header{
		Number: big.NewInt(0),
		Extra:  make([]byte, extraVanity), // too short
	})
	if err == nil {
		t.Error("expected error for extra data shorter than extraVanity+extraSeal")
	}
}

func TestNewGenesisSnapshot_BadSignerLength(t *testing.T) {
	// 1 extra byte in the signer section — not a multiple of 20.
	extra := make([]byte, extraVanity+21+extraSeal)
	_, err := NewGenesisSnapshot(&types.Header{
		Number: big.NewInt(0),
		Extra:  extra,
	})
	if err == nil {
		t.Error("expected error for non-multiple-of-20 signer bytes")
	}
}

// --- InTurn / SignerList ---

func TestSnapshot_InTurn(t *testing.T) {
	// Three signers sorted by address. Position 0 is in-turn for block 3, etc.
	k1, k2, k3 := testKey(t), testKey(t), testKey(t)
	addrs := []common.Address{addr(k1), addr(k2), addr(k3)}
	snap := makeSnap(addrs)
	list := snap.SignerList()

	// For block N: list[N%3] is in-turn.
	for n := uint64(0); n < 9; n++ {
		expected := list[n%3]
		if !snap.InTurn(n, expected) {
			t.Errorf("block %d: %s should be in-turn", n, expected)
		}
		// Others should be out-of-turn.
		for _, s := range list {
			if s != expected && snap.InTurn(n, s) {
				t.Errorf("block %d: %s should NOT be in-turn", n, s)
			}
		}
	}
}

func TestSnapshot_InTurn_NotASigner(t *testing.T) {
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(testKey(t))})
	if snap.InTurn(0, addr(k)) {
		t.Error("unknown address should never be in-turn")
	}
}

// --- HasRecentlySigned ---

func TestSnapshot_HasRecentlySigned(t *testing.T) {
	k1, k2, k3 := testKey(t), testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2), addr(k3)}
	snap := makeSnap(signers)

	// Limit for 3 signers = 3/2+1 = 2.
	// Mark k1 as having signed block 5.
	snap.Recents[5] = addr(k1)

	// At block 6: 5+2=7 > 6 → k1 recently signed.
	if !snap.HasRecentlySigned(6, addr(k1)) {
		t.Error("k1 should be blocked at block 6")
	}
	// At block 7: 5+2=7 NOT > 7 → k1 is allowed again.
	if snap.HasRecentlySigned(7, addr(k1)) {
		t.Error("k1 should be allowed at block 7")
	}
	// k2 was never in recents.
	if snap.HasRecentlySigned(6, addr(k2)) {
		t.Error("k2 was never in recents, should not be blocked")
	}
}

// --- apply: basic block replay ---

func TestSnapshot_Apply_SingleBlock(t *testing.T) {
	const epoch = 100
	engine := New(15, epoch)

	k1, k2, k3 := testKey(t), testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2), addr(k3)}
	snap := makeSnap(signers)
	list := snap.SignerList()

	// Determine which key is in-turn at block 1.
	var inTurnKey *ecdsa.PrivateKey
	switch list[1%3] {
	case addr(k1):
		inTurnKey = k1
	case addr(k2):
		inTurnKey = k2
	default:
		inTurnKey = k3
	}

	h := makeHeader(t, 1, nil, inTurnKey, snap, plainExtra(), nil)
	snap2, err := engine.Apply(snap, []*types.Header{h})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if snap2.Number != 1 {
		t.Errorf("snap2.Number = %d, want 1", snap2.Number)
	}
	if snap2.Hash != h.Hash() {
		t.Error("snap2.Hash does not match header hash")
	}
	if len(snap2.Signers) != 3 {
		t.Errorf("snap2 should still have 3 signers, got %d", len(snap2.Signers))
	}
}

func TestSnapshot_Apply_RejectsUnauthorizedSigner(t *testing.T) {
	const epoch = 100
	engine := New(15, epoch)

	snap := makeSnap([]common.Address{addr(testKey(t))})
	outsider := testKey(t)

	h := makeHeader(t, 1, nil, outsider, nil, plainExtra(), nil)
	// Force difficulty to avoid a different error first.
	h.Difficulty = big.NewInt(diffNoTurn)
	if err := SealHeader(h, outsider); err != nil {
		t.Fatal(err)
	}

	_, err := engine.Apply(snap, []*types.Header{h})
	if err == nil {
		t.Fatal("expected error for unauthorized signer")
	}
}

func TestSnapshot_Apply_RejectsRecentSigner(t *testing.T) {
	const epoch = 100
	engine := New(15, epoch)

	k1, k2 := testKey(t), testKey(t)
	snap := makeSnap([]common.Address{addr(k1), addr(k2)})
	// Limit for 2 signers = 2/2+1 = 2.
	// Pre-populate recents so k1 signed block 0.
	snap.Recents[0] = addr(k1)

	// k1 tries to sign block 1 → should be blocked (0+2 > 1).
	h := makeHeader(t, 1, nil, k1, snap, plainExtra(), nil)
	_, err := engine.Apply(snap, []*types.Header{h})
	if err == nil {
		t.Fatal("expected error: signer signed too recently")
	}
}

// --- apply: voting ---

func TestSnapshot_Apply_VoteAddSigner(t *testing.T) {
	const epoch = 100
	engine := New(15, epoch)

	// 2 signers; majority = 2/2+1 = 2 votes needed.
	k1, k2 := testKey(t), testKey(t)
	candidate := testKey(t)
	snap := makeSnap([]common.Address{addr(k1), addr(k2)})

	// Both signers vote to add the candidate.
	// k1 signs block 1 with vote for candidate.
	h1 := makeHeader(t, 1, nil, k1, snap, plainExtra(),
		&voteOpt{address: addr(candidate), authorize: true})
	snap1, err := engine.Apply(snap, []*types.Header{h1})
	if err != nil {
		t.Fatalf("Apply block 1: %v", err)
	}
	if snap1.IsAuthorized(addr(candidate)) {
		t.Error("candidate should not yet be authorized after 1 of 2 votes")
	}

	// k2 signs block 2 with vote for candidate (k1 is now in recents, so k2 must sign).
	h2 := makeHeader(t, 2, h1, k2, snap1, plainExtra(),
		&voteOpt{address: addr(candidate), authorize: true})
	snap2, err := engine.Apply(snap1, []*types.Header{h2})
	if err != nil {
		t.Fatalf("Apply block 2: %v", err)
	}
	if !snap2.IsAuthorized(addr(candidate)) {
		t.Error("candidate should be authorized after majority vote")
	}
	if len(snap2.Signers) != 3 {
		t.Errorf("expected 3 signers after adding candidate, got %d", len(snap2.Signers))
	}
	// Tally should be cleared after the vote resolved.
	if _, ok := snap2.Tally[addr(candidate)]; ok {
		t.Error("tally should be cleared after vote resolved")
	}
}

func TestSnapshot_Apply_VoteRemoveSigner(t *testing.T) {
	const epoch = 100
	engine := New(15, epoch)

	// 3 signers; majority = 3/2+1 = 2 votes needed.
	k1, k2, k3 := testKey(t), testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2), addr(k3)}
	snap := makeSnap(signers)

	// k1 votes to remove k3 (block 1).
	h1 := makeHeader(t, 1, nil, k1, snap, plainExtra(),
		&voteOpt{address: addr(k3), authorize: false})
	snap1, err := engine.Apply(snap, []*types.Header{h1})
	if err != nil {
		t.Fatalf("Apply block 1: %v", err)
	}
	if !snap1.IsAuthorized(addr(k3)) {
		t.Error("k3 should still be authorized after only 1 vote")
	}

	// k2 votes to remove k3 (block 2) → majority reached.
	h2 := makeHeader(t, 2, h1, k2, snap1, plainExtra(),
		&voteOpt{address: addr(k3), authorize: false})
	snap2, err := engine.Apply(snap1, []*types.Header{h2})
	if err != nil {
		t.Fatalf("Apply block 2: %v", err)
	}
	if snap2.IsAuthorized(addr(k3)) {
		t.Error("k3 should be deauthorized after majority vote")
	}
	if len(snap2.Signers) != 2 {
		t.Errorf("expected 2 signers after removal, got %d", len(snap2.Signers))
	}
}

// --- apply: epoch checkpoint ---

func TestSnapshot_Apply_EpochResetsVotes(t *testing.T) {
	const epoch = 5
	engine := New(15, epoch)

	k1, k2 := testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2)}
	snap := makeSnap(signers)

	// Manually add a pending vote to confirm it is cleared at the epoch.
	snap.Votes = append(snap.Votes, &Vote{
		Signer: addr(k1), Block: 3, Address: addr(k2), Authorize: false,
	})
	snap.Tally[addr(k2)] = Tally{Authorize: false, Votes: 1}

	// Apply block 5 (epoch block); extra must include signer list.
	h5 := &types.Header{
		Number:     big.NewInt(5),
		UncleHash:  types.CalcUncleHash(nil),
		Difficulty: big.NewInt(diffInTurn),
		Extra:      epochExtra(signers),
		Nonce:      nonceNull,
		Time:       75,
		MixDigest:  common.Hash{},
	}
	// Determine which key signs block 5 in-turn.
	list := snap.SignerList()
	var epochKey *ecdsa.PrivateKey
	switch list[5%2] {
	case addr(k1):
		epochKey = k1
	default:
		epochKey = k2
	}
	if !snap.InTurn(5, addr(epochKey)) {
		h5.Difficulty = big.NewInt(diffNoTurn)
	}
	if err := SealHeader(h5, epochKey); err != nil {
		t.Fatal(err)
	}

	// Advance snapshot to block 4 so block 5 follows contiguously,
	// and clear recents so neither signer is blocked.
	snap.Number = 4
	snap.Recents = map[uint64]common.Address{}

	snap5, err := engine.Apply(snap, []*types.Header{h5})
	if err != nil {
		t.Fatalf("Apply epoch block: %v", err)
	}
	if len(snap5.Votes) != 0 {
		t.Errorf("votes should be reset at epoch, got %d", len(snap5.Votes))
	}
	if len(snap5.Tally) != 0 {
		t.Errorf("tally should be reset at epoch, got %d", len(snap5.Tally))
	}
}

func TestSnapshot_Apply_NonContiguousHeaders(t *testing.T) {
	engine := New(15, 100)
	snap := makeSnap([]common.Address{addr(testKey(t))})

	h1 := &types.Header{Number: big.NewInt(1)}
	h3 := &types.Header{Number: big.NewInt(3)} // gap

	_, err := engine.Apply(snap, []*types.Header{h1, h3})
	if err == nil {
		t.Error("expected error for non-contiguous headers")
	}
}

func TestSnapshot_Copy_IsIndependent(t *testing.T) {
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	snap.Recents[1] = addr(k)
	snap.Votes = append(snap.Votes, &Vote{Signer: addr(k), Block: 1, Address: addr(k)})
	snap.Tally[addr(k)] = Tally{Authorize: true, Votes: 1}

	cpy := snap.copy()

	// Mutate the copy.
	cpy.Number = 999
	delete(cpy.Signers, addr(k))
	delete(cpy.Recents, 1)
	cpy.Votes[0].Block = 999
	delete(cpy.Tally, addr(k))

	// Original must be unaffected.
	if snap.Number != 0 {
		t.Error("original Number was mutated")
	}
	if !snap.IsAuthorized(addr(k)) {
		t.Error("original Signers was mutated")
	}
	if _, ok := snap.Recents[1]; !ok {
		t.Error("original Recents was mutated")
	}
	if snap.Votes[0].Block != 1 {
		t.Error("original Votes was mutated through copy")
	}
	if _, ok := snap.Tally[addr(k)]; !ok {
		t.Error("original Tally was mutated")
	}
}

