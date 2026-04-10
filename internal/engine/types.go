package engine

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// PayloadStatus values returned by engine_newPayload and engine_forkchoiceUpdated.
const (
	PayloadStatusValid            = "VALID"
	PayloadStatusInvalid          = "INVALID"
	PayloadStatusSyncing          = "SYNCING"
	PayloadStatusAccepted         = "ACCEPTED"
	PayloadStatusInvalidBlockHash = "INVALID_BLOCK_HASH"
)

// PayloadID is an opaque 8-byte identifier for a payload build job, returned by
// engine_forkchoiceUpdated when payloadAttributes are provided.
type PayloadID [8]byte

func (id PayloadID) MarshalText() ([]byte, error) {
	return []byte(hexutil.Encode(id[:])), nil
}

func (id *PayloadID) UnmarshalText(input []byte) error {
	b, err := hexutil.Decode(string(input))
	if err != nil {
		return fmt.Errorf("decode PayloadID: %w", err)
	}
	if len(b) != 8 {
		return fmt.Errorf("PayloadID must be 8 bytes, got %d", len(b))
	}
	copy(id[:], b)
	return nil
}

func (id PayloadID) String() string {
	return hexutil.Encode(id[:])
}

// PayloadStatusV1 is the result of engine_newPayloadV* and the payloadStatus field
// of engine_forkchoiceUpdatedV* responses.
type PayloadStatusV1 struct {
	// Status is one of the PayloadStatus* constants above.
	Status string `json:"status"`
	// LatestValidHash is the hash of the most recent valid ancestor of the
	// payload, or nil if Status is SYNCING or ACCEPTED.
	LatestValidHash *common.Hash `json:"latestValidHash"`
	// ValidationError is a human-readable description of the validation failure,
	// present when Status is INVALID.
	ValidationError *string `json:"validationError"`
}

// ForkchoiceStateV1 describes the current fork-choice view of the chain:
// the head, the "safe" head (justified), and the finalized block.
type ForkchoiceStateV1 struct {
	HeadBlockHash      common.Hash `json:"headBlockHash"`
	SafeBlockHash      common.Hash `json:"safeBlockHash"`
	FinalizedBlockHash common.Hash `json:"finalizedBlockHash"`
}

// PayloadAttributesV3 carries the parameters needed to build a new execution payload.
// Matches the EIP-4844 / Cancun version of the spec.
type PayloadAttributesV3 struct {
	Timestamp             hexutil.Uint64 `json:"timestamp"`
	PrevRandao            common.Hash    `json:"prevRandao"`
	SuggestedFeeRecipient common.Address `json:"suggestedFeeRecipient"`
	Withdrawals           []*Withdrawal  `json:"withdrawals"`
	// ParentBeaconBlockRoot is the beacon block root of the parent block.
	// For Clique networks this can be set to the zero hash.
	ParentBeaconBlockRoot *common.Hash `json:"parentBeaconBlockRoot"`
}

// Withdrawal represents an EIP-4895 validator withdrawal.
type Withdrawal struct {
	Index          hexutil.Uint64 `json:"index"`
	ValidatorIndex hexutil.Uint64 `json:"validatorIndex"`
	Address        common.Address `json:"address"`
	Amount         hexutil.Uint64 `json:"amount"`
}

// ExecutionPayloadV3 is a fully-formed execution layer block (Cancun / EIP-4844).
// For Clique networks, BlobGasUsed and ExcessBlobGas will normally be zero and
// Withdrawals will be empty.
type ExecutionPayloadV3 struct {
	ParentHash    common.Hash     `json:"parentHash"`
	FeeRecipient  common.Address  `json:"feeRecipient"`
	StateRoot     common.Hash     `json:"stateRoot"`
	ReceiptsRoot  common.Hash     `json:"receiptsRoot"`
	LogsBloom     hexutil.Bytes   `json:"logsBloom"`
	PrevRandao    common.Hash     `json:"prevRandao"`
	BlockNumber   hexutil.Uint64  `json:"blockNumber"`
	GasLimit      hexutil.Uint64  `json:"gasLimit"`
	GasUsed       hexutil.Uint64  `json:"gasUsed"`
	Timestamp     hexutil.Uint64  `json:"timestamp"`
	ExtraData     hexutil.Bytes   `json:"extraData"`
	BaseFeePerGas *hexutil.Big    `json:"baseFeePerGas"`
	BlockHash     common.Hash     `json:"blockHash"`
	Transactions  []hexutil.Bytes `json:"transactions"`
	Withdrawals   []*Withdrawal   `json:"withdrawals"`
	BlobGasUsed   hexutil.Uint64  `json:"blobGasUsed"`
	ExcessBlobGas hexutil.Uint64  `json:"excessBlobGas"`
}

// ForkchoiceUpdatedResult is the response to engine_forkchoiceUpdatedV*.
type ForkchoiceUpdatedResult struct {
	PayloadStatus PayloadStatusV1 `json:"payloadStatus"`
	// PayloadID is set when payloadAttributes were provided and payload building
	// has started. Nil otherwise.
	PayloadID *PayloadID `json:"payloadId"`
}

// GetPayloadResponseV3 is the response to engine_getPayloadV3.
type GetPayloadResponseV3 struct {
	ExecutionPayload      ExecutionPayloadV3 `json:"executionPayload"`
	BlockValue            *hexutil.Big       `json:"blockValue"`
	BlobsBundle           *BlobsBundle       `json:"blobsBundle"`
	ShouldOverrideBuilder bool               `json:"shouldOverrideBuilder"`
}

// BlobsBundle carries the KZG commitments, proofs, and blob data for EIP-4844
// transactions within a payload. For Clique networks this will be empty.
type BlobsBundle struct {
	Commitments []hexutil.Bytes `json:"commitments"`
	Proofs      []hexutil.Bytes `json:"proofs"`
	Blobs       []hexutil.Bytes `json:"blobs"`
}
