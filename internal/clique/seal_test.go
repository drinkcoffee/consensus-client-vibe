package clique

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestSigHash_Deterministic(t *testing.T) {
	h := &types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
		Extra:      plainExtra(),
		Time:       1000,
	}
	h1 := sigHash(h)
	h2 := sigHash(h)
	if h1 != h2 {
		t.Errorf("sigHash not deterministic: %v != %v", h1, h2)
	}
}

func TestSigHash_DiffersOnChange(t *testing.T) {
	base := &types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
		Extra:      plainExtra(),
		Time:       1000,
	}
	modified := &types.Header{
		Number:     big.NewInt(2), // different number
		Difficulty: big.NewInt(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
		Extra:      plainExtra(),
		Time:       1000,
	}
	if sigHash(base) == sigHash(modified) {
		t.Error("sigHash should differ for headers with different block numbers")
	}
}

func TestSigHash_ExcludesSeal(t *testing.T) {
	// Two headers identical except for the seal bytes should have the same sigHash.
	extra1 := plainExtra()
	extra2 := plainExtra()
	// Tamper with the seal region of extra2.
	extra2[extraVanity+10] = 0xff

	h1 := &types.Header{Number: big.NewInt(1), Extra: extra1, UncleHash: types.CalcUncleHash(nil)}
	h2 := &types.Header{Number: big.NewInt(1), Extra: extra2, UncleHash: types.CalcUncleHash(nil)}

	if sigHash(h1) != sigHash(h2) {
		t.Error("sigHash should be the same when only the seal bytes differ")
	}
}

func TestSealAndRecover(t *testing.T) {
	key := testKey(t)
	expected := addr(key)

	h := &types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
		Extra:      plainExtra(),
		Time:       1000,
	}

	if err := SealHeader(h, key); err != nil {
		t.Fatalf("SealHeader: %v", err)
	}

	recovered, err := SignerFromHeader(h)
	if err != nil {
		t.Fatalf("SignerFromHeader: %v", err)
	}
	if recovered != expected {
		t.Errorf("recovered signer %s, want %s", recovered, expected)
	}
}

func TestSealHeader_DifferentKeys(t *testing.T) {
	key1 := testKey(t)
	key2 := testKey(t)

	h := &types.Header{
		Number:     big.NewInt(1),
		Extra:      plainExtra(),
		UncleHash:  types.CalcUncleHash(nil),
		Difficulty: big.NewInt(diffInTurn),
	}

	if err := SealHeader(h, key1); err != nil {
		t.Fatal(err)
	}
	s1, err := SignerFromHeader(h)
	if err != nil {
		t.Fatal(err)
	}

	// Re-seal with key2 (overwrites).
	if err := SealHeader(h, key2); err != nil {
		t.Fatal(err)
	}
	s2, err := SignerFromHeader(h)
	if err != nil {
		t.Fatal(err)
	}

	if s1 == s2 {
		t.Error("different keys should produce different signers")
	}
	if s1 != addr(key1) {
		t.Errorf("first signer mismatch: got %s, want %s", s1, addr(key1))
	}
	if s2 != addr(key2) {
		t.Errorf("second signer mismatch: got %s, want %s", s2, addr(key2))
	}
}

func TestSignerFromHeader_TooShortExtra(t *testing.T) {
	h := &types.Header{Extra: make([]byte, extraSeal-1)}
	_, err := SignerFromHeader(h)
	if err == nil {
		t.Error("expected error for extra data shorter than extraSeal")
	}
}

func TestSignerFromHeader_InvalidSignature(t *testing.T) {
	// A correctly-sized but all-zero seal should fail ecrecover.
	h := &types.Header{
		Number:     big.NewInt(1),
		Extra:      plainExtra(), // all-zero seal
		UncleHash:  types.CalcUncleHash(nil),
		Difficulty: big.NewInt(diffInTurn),
	}
	_, err := SignerFromHeader(h)
	if err == nil {
		t.Error("expected error for an all-zero (invalid) seal")
	}
}
