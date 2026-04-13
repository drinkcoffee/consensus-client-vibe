package forkchoice

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// persistedBlock is the on-disk record for a single CL block.
// Header is the RLP-encoded *types.Header; rlp.RawValue embeds the bytes
// verbatim so they are not double-encoded when the struct itself is encoded.
// PayloadJSON is the JSON-encoded engine.ExecutionPayloadV3. It is stored so
// that the sync protocol can deliver payloads to peers' execution clients via
// engine_newPayloadV3 — post-merge Geth does not fetch blocks from devp2p in
// beacon mode and must receive every payload directly from its CL.
type persistedBlock struct {
	Header      rlp.RawValue
	ELHash      common.Hash
	PayloadJSON []byte // raw JSON bytes; stored as an RLP byte string (not RawValue) so arbitrary bytes round-trip correctly
}

// DBRecord is a decoded record returned by ChainDB.ReadAll.
type DBRecord struct {
	Header      *types.Header
	ELHash      common.Hash
	PayloadJSON []byte // JSON-encoded ExecutionPayloadV3; may be nil for genesis
}

// ChainDB is an append-only flat file that persists CL block headers so the
// node can rebuild its fork-choice state after a restart without re-syncing
// from peers.
//
// Format: a sequence of length-prefixed RLP records:
//
//	[4-byte big-endian length][RLP(persistedBlock)]...
//
// Records are always written in parent-before-child order (AddBlock enforces
// that the parent is already in the store), so replaying the file in order
// correctly rebuilds the store.
type ChainDB struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// OpenChainDB opens (or creates) the chain database at path. If the file ends
// with an incomplete record, the tail is silently truncated for crash safety.
// After open the write cursor is positioned at EOF ready for Append calls.
func OpenChainDB(path string) (*ChainDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create chain db directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open chain db %q: %w", path, err)
	}
	db := &ChainDB{f: f, path: path}
	if err := db.repair(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("repair chain db: %w", err)
	}
	return db, nil
}

// repair scans the file and truncates any incomplete terminal record written
// by a previous crashed run. After repair the write cursor sits at EOF.
func (db *ChainDB) repair() error {
	if _, err := db.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var goodEnd int64
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(db.f, lenBuf[:]); err != nil {
			break // io.EOF or io.ErrUnexpectedEOF — stop scanning
		}
		size := binary.BigEndian.Uint32(lenBuf[:])
		buf := make([]byte, size)
		if _, err := io.ReadFull(db.f, buf); err != nil {
			break // truncated record body
		}
		var pb persistedBlock
		if err := rlp.DecodeBytes(buf, &pb); err != nil {
			break // corrupt record
		}
		goodEnd += 4 + int64(size)
	}
	if err := db.f.Truncate(goodEnd); err != nil {
		return err
	}
	_, err := db.f.Seek(goodEnd, io.SeekStart)
	return err
}

// Append encodes header, elHash and payloadJSON as a single record and appends
// it to the file. payloadJSON may be nil (e.g. for genesis). Errors are
// non-fatal: callers log a warning and continue.
func (db *ChainDB) Append(header *types.Header, elHash common.Hash, payloadJSON []byte) error {
	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		return fmt.Errorf("encode header: %w", err)
	}
	pb := persistedBlock{Header: raw, ELHash: elHash, PayloadJSON: payloadJSON}
	payload, err := rlp.EncodeToBytes(&pb)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))

	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.f.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := db.f.Write(payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// ReadAll reads every record from the beginning of the file and returns them
// in file order (parent-before-child). After the call the write cursor is
// repositioned at EOF so subsequent Append calls work correctly.
func (db *ChainDB) ReadAll() ([]DBRecord, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if _, err := db.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var records []DBRecord
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(db.f, lenBuf[:]); err != nil {
			break
		}
		size := binary.BigEndian.Uint32(lenBuf[:])
		buf := make([]byte, size)
		if _, err := io.ReadFull(db.f, buf); err != nil {
			break
		}
		var pb persistedBlock
		if err := rlp.DecodeBytes(buf, &pb); err != nil {
			break
		}
		var h types.Header
		if err := rlp.DecodeBytes(pb.Header, &h); err != nil {
			break
		}
		records = append(records, DBRecord{Header: &h, ELHash: pb.ELHash, PayloadJSON: pb.PayloadJSON})
	}
	if _, err := db.f.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	return records, nil
}

// Truncate removes all records, resetting the database to empty. Used when
// the sync protocol detects a chain divergence requiring a resync from genesis.
func (db *ChainDB) Truncate() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.f.Truncate(0); err != nil {
		return err
	}
	_, err := db.f.Seek(0, io.SeekStart)
	return err
}

// Close releases the file handle.
func (db *ChainDB) Close() error {
	return db.f.Close()
}
