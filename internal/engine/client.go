package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
	"github.com/rs/zerolog"
)

// supportedCapabilities is the list of Engine API methods this client supports,
// sent to the execution client during the initial capabilities handshake.
var supportedCapabilities = []string{
	"engine_newPayloadV1",
	"engine_newPayloadV2",
	"engine_newPayloadV3",
	"engine_forkchoiceUpdatedV1",
	"engine_forkchoiceUpdatedV2",
	"engine_forkchoiceUpdatedV3",
	"engine_getPayloadV1",
	"engine_getPayloadV2",
	"engine_getPayloadV3",
}

// Client is a JSON-RPC client for the Engine API exposed by an execution client.
// All calls are authenticated with a JWT bearer token derived from a shared secret.
type Client struct {
	url         string
	jwt         *JWTProvider
	callTimeout time.Duration
	http        *http.Client
	log         zerolog.Logger
	reqID       atomic.Uint64
}

// New creates a new Engine API client.
//
//   - url is the HTTP endpoint of the execution client's Engine API, e.g. "http://localhost:8551".
//   - jwt is the JWT provider wrapping the shared secret.
//   - callTimeout is the per-request timeout (independent of any context deadline).
func New(url string, jwt *JWTProvider, callTimeout time.Duration) *Client {
	return &Client{
		url:         url,
		jwt:         jwt,
		callTimeout: callTimeout,
		http:        &http.Client{Timeout: callTimeout},
		log:         log.With("engine"),
	}
}

// ExchangeCapabilities performs the initial Engine API handshake. It sends the list
// of methods this consensus client supports and returns the execution client's list.
// This is also used as a connectivity health check on startup.
func (c *Client) ExchangeCapabilities(ctx context.Context) ([]string, error) {
	var result []string
	if err := c.call(ctx, "engine_exchangeCapabilities", []any{supportedCapabilities}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// NewPayloadV3 delivers a new execution payload to the execution client for validation.
//
//   - versionedHashes should be nil or empty for Clique networks (no EIP-4844 blobs).
//   - parentBeaconBlockRoot should be the zero hash for Clique networks.
func (c *Client) NewPayloadV3(
	ctx context.Context,
	payload ExecutionPayloadV3,
	versionedHashes []common.Hash,
	parentBeaconBlockRoot common.Hash,
) (PayloadStatusV1, error) {
	if versionedHashes == nil {
		versionedHashes = []common.Hash{}
	}

	var status PayloadStatusV1
	params := []any{payload, versionedHashes, parentBeaconBlockRoot}
	if err := c.call(ctx, "engine_newPayloadV3", params, &status); err != nil {
		return PayloadStatusV1{}, err
	}

	c.log.Debug().
		Str("status", status.Status).
		Str("block_hash", payload.BlockHash.Hex()).
		Uint64("block_number", uint64(payload.BlockNumber)).
		Msg("engine_newPayloadV3")

	return status, nil
}

// ForkchoiceUpdatedV3 updates the fork choice state on the execution client and,
// optionally, initiates payload building when attrs is non-nil.
//
// Returns the payload status and, if attrs were provided, a PayloadID that can be
// passed to GetPayloadV3 once the payload is ready.
func (c *Client) ForkchoiceUpdatedV3(
	ctx context.Context,
	state ForkchoiceStateV1,
	attrs *PayloadAttributesV3,
) (ForkchoiceUpdatedResult, error) {
	var result ForkchoiceUpdatedResult
	params := []any{state, attrs}
	if err := c.call(ctx, "engine_forkchoiceUpdatedV3", params, &result); err != nil {
		return ForkchoiceUpdatedResult{}, err
	}

	logEvent := c.log.Debug().
		Str("head", state.HeadBlockHash.Hex()).
		Str("safe", state.SafeBlockHash.Hex()).
		Str("finalized", state.FinalizedBlockHash.Hex()).
		Str("payload_status", result.PayloadStatus.Status)
	if result.PayloadID != nil {
		logEvent = logEvent.Str("payload_id", result.PayloadID.String())
	}
	logEvent.Msg("engine_forkchoiceUpdatedV3")

	return result, nil
}

// GetPayloadV3 retrieves a payload that was previously requested via ForkchoiceUpdatedV3.
// The caller should poll this until the execution client returns a non-SYNCING status,
// or until a block deadline is reached.
func (c *Client) GetPayloadV3(ctx context.Context, id PayloadID) (GetPayloadResponseV3, error) {
	var result GetPayloadResponseV3
	if err := c.call(ctx, "engine_getPayloadV3", []any{id}, &result); err != nil {
		return GetPayloadResponseV3{}, err
	}

	c.log.Debug().
		Str("payload_id", id.String()).
		Uint64("block_number", uint64(result.ExecutionPayload.BlockNumber)).
		Str("block_hash", result.ExecutionPayload.BlockHash.Hex()).
		Msg("engine_getPayloadV3")

	return result, nil
}

// --- JSON-RPC plumbing ---

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      uint64 `json:"id"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	Result  json.RawMessage  `json:"result"`
	Error   *jsonRPCError    `json:"error"`
	ID      uint64           `json:"id"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("engine API error %d: %s", e.Code, e.Message)
}

// call performs a single authenticated JSON-RPC request and unmarshals the result
// into out. out must be a non-nil pointer.
func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	id := c.reqID.Add(1)

	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	// Attach the per-call timeout alongside the parent context.
	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.url, bytes.NewReader(reqBytes))
	if err != nil {
		return fmt.Errorf("build HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	token, err := c.jwt.Token()
	if err != nil {
		return fmt.Errorf("get JWT token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP %s: %w", method, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, method, body)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return fmt.Errorf("unmarshal JSON-RPC response: %w", err)
	}

	if rpcResp.Error != nil {
		return rpcResp.Error
	}

	if err := json.Unmarshal(rpcResp.Result, out); err != nil {
		return fmt.Errorf("unmarshal result for %s: %w", method, err)
	}

	return nil
}
