package rpc

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/peterrobinson/consensus-client-vibe/internal/clique"
	"github.com/peterrobinson/consensus-client-vibe/internal/config"
)

// --- mock implementations ---

type mockP2P struct {
	peerID string
	addrs  []string
	peers  []peer.AddrInfo
}

func (m *mockP2P) PeerID() peer.ID {
	id, _ := peer.Decode(m.peerID)
	return id
}

func (m *mockP2P) Addrs() []ma.Multiaddr {
	addrs := make([]ma.Multiaddr, 0, len(m.addrs))
	for _, s := range m.addrs {
		a, err := ma.NewMultiaddr(s)
		if err == nil {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

func (m *mockP2P) PeerCount() int                { return len(m.peers) }
func (m *mockP2P) ConnectedPeers() []peer.AddrInfo { return m.peers }

type mockChain struct {
	head   *types.Header
	blocks map[uint64]*types.Header
	tds    map[common.Hash]*big.Int
}

func (m *mockChain) Head() *types.Header { return m.head }

func (m *mockChain) GetByNumber(n uint64) (*types.Header, bool) {
	h, ok := m.blocks[n]
	return h, ok
}

func (m *mockChain) TD(hash common.Hash) (*big.Int, bool) {
	td, ok := m.tds[hash]
	return td, ok
}

// newTestServer builds a Server backed by mock subsystems and returns an
// httptest.Server ready to serve requests.
func newTestServer(t *testing.T, p2p P2PNode, chain ChainState, snap SnapshotProvider) *httptest.Server {
	t.Helper()
	cfg := &config.RPCConfig{ListenAddr: "127.0.0.1:0"}
	srv := New(cfg, p2p, chain, snap)
	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)
	return ts
}

// defaultMocks returns a minimal set of mock subsystems with a genesis block.
func defaultMocks() (P2PNode, ChainState) {
	genesis := &types.Header{
		Number:     big.NewInt(0),
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, 97),
	}
	p := &mockP2P{
		peerID: "12D3KooWGatU9DEesagYvh8aVym6BBt7SDUDC2pwAK1zppS2HDYr",
		addrs:  []string{"/ip4/127.0.0.1/tcp/9000"},
	}
	c := &mockChain{
		head:   genesis,
		blocks: map[uint64]*types.Header{0: genesis},
		tds:    map[common.Hash]*big.Int{genesis.Hash(): big.NewInt(1)},
	}
	return p, c
}

// getJSON performs a GET and decodes the response body into out.
func getJSON(t *testing.T, ts *httptest.Server, path string, out interface{}) *http.Response {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return resp
}

// --- /eth/v1/node/identity ---

func TestNodeIdentity(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body struct {
		Data NodeIdentity `json:"data"`
	}
	resp := getJSON(t, ts, "/eth/v1/node/identity", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Data.PeerID == "" {
		t.Error("peer_id should not be empty")
	}
	if len(body.Data.P2PAddresses) == 0 {
		t.Error("p2p_addresses should not be empty")
	}
}

// --- /eth/v1/node/peers ---

func TestNodePeers_Empty(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body PeersResponse
	resp := getJSON(t, ts, "/eth/v1/node/peers", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Meta["count"] != 0 {
		t.Errorf("meta.count = %d, want 0", body.Meta["count"])
	}
	if len(body.Data) != 0 {
		t.Errorf("data len = %d, want 0", len(body.Data))
	}
}

func TestNodePeers_WithPeers(t *testing.T) {
	pid, _ := peer.Decode("12D3KooWGatU9DEesagYvh8aVym6BBt7SDUDC2pwAK1zppS2HDYr")
	addr, _ := ma.NewMultiaddr("/ip4/192.0.2.1/tcp/9000")

	p2p := &mockP2P{
		peerID: "12D3KooWMZVe9t3AtUp3P8vjEycXf4p9Hnq1dZx2CX1dFkWiYooh",
		peers:  []peer.AddrInfo{{ID: pid, Addrs: []ma.Multiaddr{addr}}},
	}
	_, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body PeersResponse
	resp := getJSON(t, ts, "/eth/v1/node/peers", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Meta["count"] != 1 {
		t.Errorf("meta.count = %d, want 1", body.Meta["count"])
	}
	if len(body.Data) != 1 {
		t.Fatalf("data len = %d, want 1", len(body.Data))
	}
	if body.Data[0].State != "connected" {
		t.Errorf("state = %q, want connected", body.Data[0].State)
	}
}

// --- /eth/v1/node/health ---

func TestNodeHealth_WithHead(t *testing.T) {
	p2p := &mockP2P{
		peerID: "12D3KooWGatU9DEesagYvh8aVym6BBt7SDUDC2pwAK1zppS2HDYr",
		peers:  []peer.AddrInfo{{ID: peer.ID("dummy")}}, // non-empty peer list
	}
	_, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Get(ts.URL + "/eth/v1/node/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
}

func TestNodeHealth_NoPeers(t *testing.T) {
	p2p, chain := defaultMocks() // 0 peers
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Get(ts.URL + "/eth/v1/node/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

func TestNodeHealth_NoHead(t *testing.T) {
	p2p := &mockP2P{peers: []peer.AddrInfo{{ID: peer.ID("dummy")}}}
	chain := &mockChain{} // nil head
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Get(ts.URL + "/eth/v1/node/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

// --- /eth/v1/node/syncing ---

func TestNodeSyncing(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body struct {
		Data SyncStatus `json:"data"`
	}
	resp := getJSON(t, ts, "/eth/v1/node/syncing", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Data.IsSyncing {
		t.Error("is_syncing should be false")
	}
	if body.Data.HeadSlot != "0" {
		t.Errorf("head_slot = %q, want \"0\"", body.Data.HeadSlot)
	}
}

// --- /clique/v1/head ---

func TestCliqueHead(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body struct {
		Data HeadInfo `json:"data"`
	}
	resp := getJSON(t, ts, "/clique/v1/head", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Data.Number != "0" {
		t.Errorf("number = %q, want \"0\"", body.Data.Number)
	}
	if body.Data.Hash == "" {
		t.Error("hash should not be empty")
	}
	if body.Data.TotalDifficulty != "1" {
		t.Errorf("total_difficulty = %q, want \"1\"", body.Data.TotalDifficulty)
	}
}

func TestCliqueHead_NoHead(t *testing.T) {
	p2p, _ := defaultMocks()
	chain := &mockChain{}
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Get(ts.URL + "/clique/v1/head")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

// --- /clique/v1/validators ---

func TestCliqueValidators_NoSnapshot(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil) // nil snap

	var body struct {
		Data ValidatorsInfo `json:"data"`
	}
	resp := getJSON(t, ts, "/clique/v1/validators", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Data.Count != 0 {
		t.Errorf("count = %d, want 0", body.Data.Count)
	}
}

func TestCliqueValidators_WithSnapshot(t *testing.T) {
	p2p, chain := defaultMocks()

	addr1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	addr2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	snap := &clique.Snapshot{
		Signers: map[common.Address]struct{}{
			addr1: {},
			addr2: {},
		},
		Recents: map[uint64]common.Address{},
		Tally:   map[common.Address]clique.Tally{},
	}
	snapFn := func() *clique.Snapshot { return snap }
	ts := newTestServer(t, p2p, chain, snapFn)

	var body struct {
		Data ValidatorsInfo `json:"data"`
	}
	resp := getJSON(t, ts, "/clique/v1/validators", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Data.Count != 2 {
		t.Errorf("count = %d, want 2", body.Data.Count)
	}
	if len(body.Data.Signers) != 2 {
		t.Errorf("signers len = %d, want 2", len(body.Data.Signers))
	}
}

// --- /clique/v1/blocks/{number} ---

func TestCliqueBlock_Found(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body struct {
		Data BlockInfo `json:"data"`
	}
	resp := getJSON(t, ts, "/clique/v1/blocks/0", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if body.Data.Number != "0" {
		t.Errorf("number = %q, want \"0\"", body.Data.Number)
	}
}

func TestCliqueBlock_NotFound(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Get(ts.URL + "/clique/v1/blocks/9999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func TestCliqueBlock_BadNumber(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Get(ts.URL + "/clique/v1/blocks/notanumber")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

// --- /clique/v1/votes ---

func TestCliqueVotes_Empty(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	var body struct {
		Data []VoteInfo `json:"data"`
	}
	resp := getJSON(t, ts, "/clique/v1/votes", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

func TestCliqueVotes_WithVotes(t *testing.T) {
	p2p, chain := defaultMocks()

	signer := common.HexToAddress("0xaaaa")
	target := common.HexToAddress("0xbbbb")
	snap := &clique.Snapshot{
		Signers: map[common.Address]struct{}{signer: {}},
		Recents: map[uint64]common.Address{},
		Tally:   map[common.Address]clique.Tally{},
		Votes: []*clique.Vote{
			{Signer: signer, Address: target, Authorize: true, Block: 3},
		},
	}
	ts := newTestServer(t, p2p, chain, func() *clique.Snapshot { return snap })

	var body struct {
		Data []VoteInfo `json:"data"`
	}
	resp := getJSON(t, ts, "/clique/v1/votes", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if len(body.Data) != 1 {
		t.Fatalf("votes len = %d, want 1", len(body.Data))
	}
	if !body.Data[0].Authorize {
		t.Error("authorize should be true")
	}
	if body.Data[0].Block != "3" {
		t.Errorf("block = %q, want \"3\"", body.Data[0].Block)
	}
}

// --- POST /clique/v1/vote ---

func TestPostVote_Valid(t *testing.T) {
	p2p, chain := defaultMocks()
	cfg := &config.RPCConfig{ListenAddr: "127.0.0.1:0"}
	srv := New(cfg, p2p, chain, nil)
	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)

	body := `{"address":"0x1234567890123456789012345678901234567890","authorize":true}`
	resp, err := http.Post(ts.URL+"/clique/v1/vote", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}

	pv := srv.PendingVote()
	if pv == nil {
		t.Fatal("expected pending vote, got nil")
	}
	if pv.Address.Hex() != "0x1234567890123456789012345678901234567890" {
		t.Errorf("address = %s", pv.Address.Hex())
	}
	if !pv.Authorize {
		t.Error("authorize should be true")
	}

	// Consuming pending vote clears it.
	if srv.PendingVote() != nil {
		t.Error("pending vote should be nil after consumption")
	}
}

func TestPostVote_InvalidAddress(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	body := `{"address":"notanaddress","authorize":true}`
	resp, err := http.Post(ts.URL+"/clique/v1/vote", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestPostVote_ZeroAddress(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	body := `{"address":"0x0000000000000000000000000000000000000000","authorize":true}`
	resp, err := http.Post(ts.URL+"/clique/v1/vote", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestPostVote_BadJSON(t *testing.T) {
	p2p, chain := defaultMocks()
	ts := newTestServer(t, p2p, chain, nil)

	resp, err := http.Post(ts.URL+"/clique/v1/vote", "application/json", strings.NewReader("{bad"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}
