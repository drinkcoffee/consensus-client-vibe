package clique

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestCalcDifficulty_InTurn(t *testing.T) {
	k1, k2 := testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2)}
	snap := makeSnap(signers)
	list := snap.SignerList()
	engine := New(15, 100)

	// For block 1: list[1%2] is in-turn.
	inTurnSigner := list[1%2]
	diff := engine.CalcDifficulty(snap, 1, inTurnSigner)
	if diff.Int64() != diffInTurn {
		t.Errorf("in-turn difficulty = %v, want %d", diff, diffInTurn)
	}
}

func TestCalcDifficulty_OutOfTurn(t *testing.T) {
	k1, k2 := testKey(t), testKey(t)
	signers := []common.Address{addr(k1), addr(k2)}
	snap := makeSnap(signers)
	list := snap.SignerList()
	engine := New(15, 100)

	// The signer at list[0] is out-of-turn for block 1.
	outOfTurnSigner := list[0]
	diff := engine.CalcDifficulty(snap, 1, outOfTurnSigner)
	if diff.Int64() != diffNoTurn {
		t.Errorf("out-of-turn difficulty = %v, want %d", diff, diffNoTurn)
	}
}

func TestVerifyHeader_Valid(t *testing.T) {
	const period = 15
	engine := New(period, 100)

	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})

	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}
	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)

	if err := engine.VerifyHeader(snap, h, parent); err != nil {
		t.Errorf("valid header rejected: %v", err)
	}
}

func TestVerifyHeader_UnauthorizedSigner(t *testing.T) {
	engine := New(15, 100)
	snap := makeSnap([]common.Address{addr(testKey(t))})
	outsider := testKey(t)

	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}
	h := makeHeader(t, 1, parent, outsider, nil, plainExtra(), nil)
	h.Difficulty = big.NewInt(diffInTurn)
	if err := SealHeader(h, outsider); err != nil {
		t.Fatal(err)
	}

	err := engine.VerifyHeader(snap, h, parent)
	if err == nil {
		t.Fatal("expected error for unauthorized signer")
	}
}

func TestVerifyHeader_InvalidNonce(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})

	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}
	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	// Corrupt the nonce to an invalid value.
	h.Nonce = types.BlockNonce{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	// Re-seal so the signature still matches.
	if err := SealHeader(h, k); err != nil {
		t.Fatal(err)
	}

	err := engine.VerifyHeader(snap, h, parent)
	if err == nil {
		t.Fatal("expected error for invalid nonce")
	}
}

func TestVerifyHeader_InvalidMixDigest(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})

	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}
	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	h.MixDigest = common.HexToHash("0xdeadbeef")
	if err := SealHeader(h, k); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for non-zero mix digest")
	}
}

func TestVerifyHeader_InvalidUncleHash(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})

	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}
	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	h.UncleHash = common.HexToHash("0xbad")
	if err := SealHeader(h, k); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for wrong uncle hash")
	}
}

func TestVerifyHeader_TimestampTooEarly(t *testing.T) {
	const period = 15
	engine := New(period, 100)

	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}

	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	h.Time = parent.Time + period - 1 // one second too early
	if err := SealHeader(h, k); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for timestamp too early")
	}
}

func TestVerifyHeader_WrongDifficulty(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}

	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	// Flip the difficulty.
	if h.Difficulty.Int64() == diffInTurn {
		h.Difficulty = big.NewInt(diffNoTurn)
	} else {
		h.Difficulty = big.NewInt(diffInTurn)
	}
	if err := SealHeader(h, k); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for wrong difficulty")
	}
}

func TestVerifyHeader_ExtraDataTooShort(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}

	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	h.Extra = make([]byte, extraVanity) // missing seal bytes
	// Don't bother re-sealing; the length check happens first.

	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for short extra data")
	}
}

func TestVerifyHeader_NonEpochWithSignerList(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	signers := []common.Address{addr(k)}
	snap := makeSnap(signers)
	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}

	// Block 1 is not an epoch block but we include a signer list in extra.
	h := makeHeader(t, 1, parent, k, snap, epochExtra(signers), nil)
	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for signer list in non-epoch block")
	}
}

func TestVerifyHeader_EpochMissingSignerList(t *testing.T) {
	const epoch = 5
	engine := New(15, epoch)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	parent := &types.Header{Time: 1000 + 4*15, Number: big.NewInt(4)}

	// Block 5 is an epoch block; extra should contain the signer list.
	h := makeHeader(t, 5, parent, k, snap, plainExtra(), nil) // no signer list
	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error for missing signer list at epoch block")
	}
}

func TestVerifyHeader_AuthorizeVoteNonZeroCoinbase(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	candidate := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}

	// A vote to authorize candidate should be accepted.
	h := makeHeader(t, 1, parent, k, snap, plainExtra(),
		&voteOpt{address: addr(candidate), authorize: true})
	if err := engine.VerifyHeader(snap, h, parent); err != nil {
		t.Errorf("valid authorize vote rejected: %v", err)
	}
}

func TestVerifyHeader_AuthorizeVoteZeroCoinbase(t *testing.T) {
	engine := New(15, 100)
	k := testKey(t)
	snap := makeSnap([]common.Address{addr(k)})
	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}

	h := makeHeader(t, 1, parent, k, snap, plainExtra(), nil)
	// Force authorize nonce with zero coinbase — should be rejected.
	h.Coinbase = common.Address{}
	h.Nonce = nonceAuth
	if err := SealHeader(h, k); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyHeader(snap, h, parent); err == nil {
		t.Fatal("expected error: authorize nonce with zero beneficiary")
	}
}

func TestVerifyHeader_RecentlySigned(t *testing.T) {
	engine := New(15, 100)
	k1, k2 := testKey(t), testKey(t)
	snap := makeSnap([]common.Address{addr(k1), addr(k2)})

	// k1 already signed block 0; limit = 2, so k1 cannot sign block 1.
	snap.Recents[0] = addr(k1)

	parent := &types.Header{Time: 1000, Number: big.NewInt(0)}
	h := makeHeader(t, 1, parent, k1, snap, plainExtra(), nil)

	err := engine.VerifyHeader(snap, h, parent)
	if err == nil {
		t.Fatal("expected error: signer signed too recently")
	}
}
