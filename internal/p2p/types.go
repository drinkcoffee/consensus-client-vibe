// Package p2p implements the Clique consensus client's peer-to-peer networking
// layer on top of libp2p.
//
// Wire protocol:
//
//   /clique/block/1   — Gossipsub topic carrying signed CliqueBlock messages.
//   /clique/status/1  — Stream protocol for the peer handshake (StatusMsg
//                       exchange) performed when a new outbound connection is
//                       established.
//
// Message encoding: all messages are RLP-encoded. Status stream messages are
// further framed with a 4-byte big-endian length prefix so that they can be
// read back from a streaming connection.
package p2p

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
)

// CliqueBlock is the Gossipsub wire type for propagating signed block headers.
// The header is stored as a raw RLP byte slice so callers can decode it into
// a *types.Header with a single rlp.DecodeBytes call.
//
// The full execution payload is included so that receiving nodes can deliver
// it to their local execution client via engine_newPayloadV3. This mirrors
// how Ethereum beacon blocks include the full ExecutionPayload, ensuring that
// each node's EL has the block before the next production slot fires.
type CliqueBlock struct {
	// Header is the RLP-encoded *types.Header (including the 65-byte seal in
	// the trailing bytes of Extra).
	Header rlp.RawValue
	// ExecutionPayloadHash is the EL block hash — a convenience field equal to
	// the BlockHash field inside PayloadJSON. Kept for fast access without
	// a full JSON decode.
	ExecutionPayloadHash common.Hash
	// PayloadJSON is the JSON-encoded engine.ExecutionPayloadV3. Receiving
	// nodes pass this directly to engine_newPayloadV3 on their local EL.
	PayloadJSON []byte
}

// NewCliqueBlock creates a CliqueBlock from a CL header and full execution payload.
func NewCliqueBlock(h *types.Header, payload engine.ExecutionPayloadV3) (*CliqueBlock, error) {
	raw, err := rlp.EncodeToBytes(h)
	if err != nil {
		return nil, fmt.Errorf("RLP encode header: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("JSON encode payload: %w", err)
	}
	return &CliqueBlock{
		Header:               raw,
		ExecutionPayloadHash: payload.BlockHash,
		PayloadJSON:          payloadJSON,
	}, nil
}

// DecodePayload decodes the execution payload from its JSON representation.
func (b *CliqueBlock) DecodePayload() (*engine.ExecutionPayloadV3, error) {
	var p engine.ExecutionPayloadV3
	if err := json.Unmarshal(b.PayloadJSON, &p); err != nil {
		return nil, fmt.Errorf("JSON decode payload: %w", err)
	}
	return &p, nil
}

// DecodeHeader decodes the embedded header from its raw RLP representation.
func (b *CliqueBlock) DecodeHeader() (*types.Header, error) {
	var h types.Header
	if err := rlp.DecodeBytes(b.Header, &h); err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	return &h, nil
}

// Encode serialises the CliqueBlock to RLP for transmission over Gossipsub.
func (b *CliqueBlock) Encode() ([]byte, error) {
	data, err := rlp.EncodeToBytes(b)
	if err != nil {
		return nil, fmt.Errorf("encode CliqueBlock: %w", err)
	}
	return data, nil
}

// DecodeCliqueBlock deserialises a CliqueBlock from RLP bytes.
func DecodeCliqueBlock(data []byte) (*CliqueBlock, error) {
	var b CliqueBlock
	if err := rlp.DecodeBytes(data, &b); err != nil {
		return nil, fmt.Errorf("decode CliqueBlock: %w", err)
	}
	return &b, nil
}

// StatusMsg is exchanged with every new peer to verify network compatibility.
// A peer with a different NetworkID or GenesisHash is disconnected.
type StatusMsg struct {
	NetworkID   uint64
	GenesisHash common.Hash
	HeadHash    common.Hash
	HeadNumber  uint64
}

// writeMsg writes a length-prefixed RLP message to w.
// Format: [4-byte big-endian length][RLP payload].
func writeMsg(w io.Writer, msg interface{}) error {
	payload, err := rlp.EncodeToBytes(msg)
	if err != nil {
		return fmt.Errorf("RLP encode: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// readMsg reads a length-prefixed RLP message from r and decodes it into out.
// maxSize limits the payload length to guard against malformed messages.
func readMsg(r io.Reader, out interface{}, maxSize uint32) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return fmt.Errorf("read length prefix: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxSize {
		return fmt.Errorf("message too large: %d bytes (limit %d)", n, maxSize)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	if err := rlp.DecodeBytes(payload, out); err != nil {
		return fmt.Errorf("RLP decode: %w", err)
	}
	return nil
}
