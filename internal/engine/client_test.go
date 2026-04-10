package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// testJWTProvider creates a JWTProvider backed by a temporary 32-byte secret.
func testJWTProvider(t *testing.T) *JWTProvider {
	t.Helper()
	secret := make([]byte, 32)
	f := filepath.Join(t.TempDir(), "jwt.hex")
	if err := os.WriteFile(f, []byte(hex.EncodeToString(secret)), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := NewJWTProvider(f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// mockServer starts an httptest server that responds to JSON-RPC requests with the
// provided handler function and returns the client pointed at it.
func mockServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, testJWTProvider(t), 5*time.Second)
}

// rpcResponse writes a successful JSON-RPC response envelope with the given result.
func rpcResponse(w http.ResponseWriter, id uint64, result any) {
	raw, _ := json.Marshal(result)
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  json.RawMessage(raw),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// decodeRequest decodes the JSON-RPC request from r and returns it.
func decodeRequest(t *testing.T, r *http.Request) jsonRPCRequest {
	t.Helper()
	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func TestExchangeCapabilities(t *testing.T) {
	want := []string{"engine_newPayloadV3", "engine_forkchoiceUpdatedV3"}

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req.Method != "engine_exchangeCapabilities" {
			t.Errorf("unexpected method %q", req.Method)
		}
		// Verify the Authorization header is present.
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization header")
		}
		rpcResponse(w, req.ID, want)
	})

	got, err := client.ExchangeCapabilities(context.Background())
	if err != nil {
		t.Fatalf("ExchangeCapabilities: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("got %d capabilities, want %d", len(got), len(want))
	}
}

func TestNewPayloadV3(t *testing.T) {
	blockHash := common.HexToHash("0xdeadbeef")
	status := PayloadStatusV1{Status: PayloadStatusValid, LatestValidHash: &blockHash}

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req.Method != "engine_newPayloadV3" {
			t.Errorf("unexpected method %q", req.Method)
		}
		rpcResponse(w, req.ID, status)
	})

	payload := ExecutionPayloadV3{
		BlockHash:   blockHash,
		BlockNumber: hexutil.Uint64(1),
		Withdrawals: []*Withdrawal{},
	}

	got, err := client.NewPayloadV3(context.Background(), payload, nil, common.Hash{})
	if err != nil {
		t.Fatalf("NewPayloadV3: %v", err)
	}
	if got.Status != PayloadStatusValid {
		t.Errorf("status = %q, want %q", got.Status, PayloadStatusValid)
	}
}

func TestForkchoiceUpdatedV3_NoAttrs(t *testing.T) {
	payStatus := PayloadStatusV1{Status: PayloadStatusValid}
	want := ForkchoiceUpdatedResult{PayloadStatus: payStatus, PayloadID: nil}

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req.Method != "engine_forkchoiceUpdatedV3" {
			t.Errorf("unexpected method %q", req.Method)
		}
		rpcResponse(w, req.ID, want)
	})

	state := ForkchoiceStateV1{
		HeadBlockHash:      common.HexToHash("0x01"),
		SafeBlockHash:      common.HexToHash("0x01"),
		FinalizedBlockHash: common.HexToHash("0x01"),
	}

	got, err := client.ForkchoiceUpdatedV3(context.Background(), state, nil)
	if err != nil {
		t.Fatalf("ForkchoiceUpdatedV3: %v", err)
	}
	if got.PayloadStatus.Status != PayloadStatusValid {
		t.Errorf("status = %q, want %q", got.PayloadStatus.Status, PayloadStatusValid)
	}
	if got.PayloadID != nil {
		t.Errorf("expected nil PayloadID, got %v", got.PayloadID)
	}
}

func TestForkchoiceUpdatedV3_WithAttrs(t *testing.T) {
	var id PayloadID
	id[0] = 0xab
	payStatus := PayloadStatusV1{Status: PayloadStatusValid}
	want := ForkchoiceUpdatedResult{PayloadStatus: payStatus, PayloadID: &id}

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		rpcResponse(w, req.ID, want)
	})

	state := ForkchoiceStateV1{}
	attrs := &PayloadAttributesV3{
		Timestamp:   hexutil.Uint64(1234),
		Withdrawals: []*Withdrawal{},
	}

	got, err := client.ForkchoiceUpdatedV3(context.Background(), state, attrs)
	if err != nil {
		t.Fatalf("ForkchoiceUpdatedV3: %v", err)
	}
	if got.PayloadID == nil {
		t.Fatal("expected non-nil PayloadID")
	}
	if *got.PayloadID != id {
		t.Errorf("PayloadID = %v, want %v", got.PayloadID, id)
	}
}

func TestGetPayloadV3(t *testing.T) {
	blockHash := common.HexToHash("0xcafe")
	want := GetPayloadResponseV3{
		ExecutionPayload: ExecutionPayloadV3{
			BlockHash:   blockHash,
			BlockNumber: hexutil.Uint64(42),
			Withdrawals: []*Withdrawal{},
		},
		BlobsBundle: &BlobsBundle{
			Commitments: []hexutil.Bytes{},
			Proofs:      []hexutil.Bytes{},
			Blobs:       []hexutil.Bytes{},
		},
	}

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req.Method != "engine_getPayloadV3" {
			t.Errorf("unexpected method %q", req.Method)
		}
		rpcResponse(w, req.ID, want)
	})

	var id PayloadID
	id[0] = 0x01

	got, err := client.GetPayloadV3(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPayloadV3: %v", err)
	}
	if got.ExecutionPayload.BlockHash != blockHash {
		t.Errorf("block hash = %v, want %v", got.ExecutionPayload.BlockHash, blockHash)
	}
	if uint64(got.ExecutionPayload.BlockNumber) != 42 {
		t.Errorf("block number = %d, want 42", got.ExecutionPayload.BlockNumber)
	}
}

func TestRPCError(t *testing.T) {
	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "method not found",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	_, err := client.ExchangeCapabilities(context.Background())
	if err == nil {
		t.Fatal("expected error from RPC error response")
	}
}

// TestCallTimeout verifies that the client returns an error when the execution
// client takes longer to respond than the configured call timeout.
// The handler sleeps longer than the client timeout so the client gives up first;
// the handler exits on its own before t.Cleanup closes the server.
func TestCallTimeout(t *testing.T) {
	const handlerDelay = 300 * time.Millisecond
	const clientTimeout = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(handlerDelay)
	}))
	defer srv.Close()

	client := New(srv.URL, testJWTProvider(t), clientTimeout)

	_, err := client.ExchangeCapabilities(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
