package p2p

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
)

// --- CliqueBlock ---

func TestCliqueBlock_EncodeDecodeRoundtrip(t *testing.T) {
	h := &types.Header{
		Number:     big.NewInt(42),
		Difficulty: big.NewInt(2),
		Extra:      make([]byte, 97), // vanity(32) + seal(65)
	}
	payloadHash := common.HexToHash("0xdeadbeef")
	payload := engine.ExecutionPayloadV3{BlockHash: payloadHash}

	blk, err := NewCliqueBlock(h, payload)
	if err != nil {
		t.Fatalf("NewCliqueBlock: %v", err)
	}

	// Encode → decode roundtrip.
	data, err := blk.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	blk2, err := DecodeCliqueBlock(data)
	if err != nil {
		t.Fatalf("DecodeCliqueBlock: %v", err)
	}

	// Compare payload hash (convenience field).
	if blk2.ExecutionPayloadHash != payloadHash {
		t.Errorf("ExecutionPayloadHash: got %s, want %s",
			blk2.ExecutionPayloadHash.Hex(), payloadHash.Hex())
	}

	// Decode and compare the full payload.
	p2, err := blk2.DecodePayload()
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if p2.BlockHash != payloadHash {
		t.Errorf("payload BlockHash: got %s, want %s", p2.BlockHash.Hex(), payloadHash.Hex())
	}

	// Decode the embedded header and compare number.
	h2, err := blk2.DecodeHeader()
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if h2.Number.Cmp(h.Number) != 0 {
		t.Errorf("header Number: got %v, want %v", h2.Number, h.Number)
	}
}

func TestCliqueBlock_RawHeaderPreserved(t *testing.T) {
	h := &types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, 97),
	}
	payload := engine.ExecutionPayloadV3{}

	blk, err := NewCliqueBlock(h, payload)
	if err != nil {
		t.Fatal(err)
	}

	// The raw bytes of the embedded header must be stable (same if encoded again).
	blk2, err := NewCliqueBlock(h, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blk.Header, blk2.Header) {
		t.Error("raw header bytes differ between two encodings of the same header")
	}
}

func TestDecodeCliqueBlock_Invalid(t *testing.T) {
	_, err := DecodeCliqueBlock([]byte{0xff, 0xfe})
	if err == nil {
		t.Error("expected error for malformed RLP input")
	}
}

// --- StatusMsg ---

func TestStatusMsg_WriteThenRead(t *testing.T) {
	msg := StatusMsg{
		NetworkID:   999,
		GenesisHash: common.HexToHash("0xaabbcc"),
		HeadHash:    common.HexToHash("0x112233"),
		HeadNumber:  1000,
	}

	var buf bytes.Buffer
	if err := writeMsg(&buf, &msg); err != nil {
		t.Fatalf("writeMsg: %v", err)
	}

	var got StatusMsg
	if err := readMsg(&buf, &got, maxStatusMsgSize); err != nil {
		t.Fatalf("readMsg: %v", err)
	}

	if got.NetworkID != msg.NetworkID {
		t.Errorf("NetworkID: got %d, want %d", got.NetworkID, msg.NetworkID)
	}
	if got.GenesisHash != msg.GenesisHash {
		t.Errorf("GenesisHash: got %s, want %s", got.GenesisHash.Hex(), msg.GenesisHash.Hex())
	}
	if got.HeadHash != msg.HeadHash {
		t.Errorf("HeadHash: got %s, want %s", got.HeadHash.Hex(), msg.HeadHash.Hex())
	}
	if got.HeadNumber != msg.HeadNumber {
		t.Errorf("HeadNumber: got %d, want %d", got.HeadNumber, msg.HeadNumber)
	}
}

func TestReadMsg_TooLarge(t *testing.T) {
	msg := StatusMsg{NetworkID: 1}

	var buf bytes.Buffer
	if err := writeMsg(&buf, &msg); err != nil {
		t.Fatal(err)
	}

	// Set maxSize to 0 — any non-empty message should be rejected.
	var got StatusMsg
	err := readMsg(&buf, &got, 0)
	if err == nil {
		t.Error("expected error for oversized message")
	}
}

func TestReadMsg_Truncated(t *testing.T) {
	msg := StatusMsg{NetworkID: 1}

	var buf bytes.Buffer
	if err := writeMsg(&buf, &msg); err != nil {
		t.Fatal(err)
	}

	// Trim the last byte so the payload is incomplete.
	truncated := buf.Bytes()[:buf.Len()-1]

	var got StatusMsg
	err := readMsg(bytes.NewReader(truncated), &got, maxStatusMsgSize)
	if err == nil {
		t.Error("expected error for truncated payload")
	}
}
