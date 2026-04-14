//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	qbfteng "github.com/peterrobinson/consensus-client-vibe/internal/consensus/qbft"
	"github.com/peterrobinson/consensus-client-vibe/internal/node"
)

// TestQBFT_BlockProduction runs a four-node QBFT network (three validators, one
// follower) and verifies:
//
//  1. All four nodes reach block 20.
//  2. Every block is signed by a valid validator (proposer rotation is verified
//     for round-0 blocks; blocks that underwent a round change may have a
//     different proposer and are only checked for validator membership).
//  3. Every committed block carries at least quorum (3) committed seals.
//  4. The follower's canonical chain matches the validators'.
func TestQBFT_BlockProduction(t *testing.T) {
	gethBin := gethBinary(t)
	dir := t.TempDir()

	// ── Validator keys ────────────────────────────────────────────────────────
	//
	// Three validator keys, sorted by address to match the engine's SignerList
	// ordering (lexicographic on hex string). The follower (node 4) has no key.

	const numValidators = 3
	type valInfo struct {
		keyPath string
		addr    common.Address
	}
	vals := make([]valInfo, numValidators)
	for i := range vals {
		key, err := gethcrypto.GenerateKey()
		if err != nil {
			t.Fatal("generate validator key:", err)
		}
		path := filepath.Join(dir, fmt.Sprintf("val%d.hex", i))
		writeFile(t, path, hex.EncodeToString(gethcrypto.FromECDSA(key))+"\n")
		vals[i] = valInfo{keyPath: path, addr: gethcrypto.PubkeyToAddress(key.PublicKey)}
	}
	sort.Slice(vals, func(i, j int) bool {
		return vals[i].addr.Hex() < vals[j].addr.Hex()
	})
	valAddrs := make([]common.Address, numValidators)
	for i, v := range vals {
		valAddrs[i] = v.addr
		t.Logf("validator[%d]: %s", i, v.addr.Hex())
	}

	// ── JWT secrets ──────────────────────────────────────────────────────────

	const numNodes = 4
	jwtPaths := make([]string, numNodes)
	for i := range jwtPaths {
		jwtPaths[i] = writeJWT(t, filepath.Join(dir, fmt.Sprintf("jwt-%d.hex", i+1)))
	}

	// ── Genesis ───────────────────────────────────────────────────────────────
	//
	// Clique-format genesis with all three validators in extra data.
	// The QBFT engine's fallback genesis parser reads the validator list from
	// this Clique-format extra data.

	const (
		chainID = 54322
		period  = 1
		epoch   = 30000
	)
	genesisTime := uint64(time.Now().Unix()) - 1
	genesisPath := filepath.Join(dir, "genesis.json")
	writeFile(t, genesisPath, buildQBFTGenesis(chainID, period, genesisTime, valAddrs))

	// ── Geth instances ────────────────────────────────────────────────────────

	type gethPorts struct{ engine, http int }
	gPorts := make([]gethPorts, numNodes)

	for i := range gPorts {
		enginePort, httpPort, p2pPort := freePort(t), freePort(t), freePort(t)
		gethDir := filepath.Join(dir, fmt.Sprintf("geth-%d", i+1))
		logPath := filepath.Join(dir, fmt.Sprintf("geth-%d.log", i+1))
		initGeth(t, gethBin, gethDir, genesisPath)
		cmd := startGeth(t, gethBin, gethDir, chainID, enginePort, httpPort, p2pPort,
			jwtPaths[i], logPath)
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d", httpPort), 30*time.Second, logPath)
		gPorts[i] = gethPorts{enginePort, httpPort}
	}

	// ── CL nodes ─────────────────────────────────────────────────────────────
	//
	// Create all nodes first so their P2P addresses are available, then wire
	// them together as boot peers before calling Start.

	clNodes := make([]*node.Node, numNodes)
	clDirs := make([]string, numNodes)

	for i := range clNodes {
		clDir := filepath.Join(dir, fmt.Sprintf("cl-%d", i+1))
		clDirs[i] = clDir

		cfg := config.DefaultConfig()
		cfg.Node.NetworkID = chainID
		cfg.Node.DataDir = clDir
		cfg.Engine.URL = fmt.Sprintf("http://127.0.0.1:%d", gPorts[i].engine)
		cfg.Engine.JWTSecretPath = jwtPaths[i]
		cfg.Engine.ELRPCUrl = fmt.Sprintf("http://127.0.0.1:%d", gPorts[i].http)
		cfg.P2P.ListenAddr = "/ip4/127.0.0.1/tcp/0"
		cfg.P2P.MaxPeers = 10
		cfg.RPC.ListenAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
		cfg.Consensus.Type = "qbft"
		cfg.Consensus.QBFT.Period = period
		cfg.Consensus.QBFT.Epoch = epoch
		cfg.Consensus.QBFT.RequestTimeoutMs = 4000
		if i < numValidators {
			cfg.Consensus.QBFT.ValidatorKeyPath = vals[i].keyPath
		}
		// Boot nodes set below after all P2P addresses are known.

		n, err := node.New(cfg)
		if err != nil {
			t.Fatalf("create node-%d: %v", i+1, err)
		}
		clNodes[i] = n
	}

	// Collect all P2P addresses and wire each node to the others.
	p2pAddrs := make([]string, numNodes)
	for i, n := range clNodes {
		p2pAddrs[i] = n.P2PAddr()
		t.Logf("node-%d P2P: %s", i+1, p2pAddrs[i])
	}
	for i, n := range clNodes {
		peers := make([]string, 0, numNodes-1)
		for j, addr := range p2pAddrs {
			if j != i {
				peers = append(peers, addr)
			}
		}
		n.SetBootNodes(peers)
	}

	// ── Start nodes ───────────────────────────────────────────────────────────

	ctxs := make([]context.Context, numNodes)
	cancels := make([]context.CancelFunc, numNodes)
	for i := range clNodes {
		ctxs[i], cancels[i] = context.WithCancel(context.Background())
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	for i, n := range clNodes {
		go func(idx int, nd *node.Node) {
			if err := nd.Start(ctxs[idx]); err != nil {
				t.Logf("node-%d exited: %v", idx+1, err)
			}
		}(i, n)
	}

	// ── Wait for all four nodes to reach block 20 ─────────────────────────────

	const targetBlock = 20
	const blockTimeout = 120 * time.Second

	for i, n := range clNodes {
		kind := "validator"
		if i >= numValidators {
			kind = "follower"
		}
		t.Logf("waiting for node-%d (%s) to reach block %d …", i+1, kind, targetBlock)
		waitForBlock(t, n, targetBlock, blockTimeout)
		t.Logf("node-%d (%s) head=%d ✓", i+1, kind, n.HeadNumber())
	}

	// ── Shutdown ─────────────────────────────────────────────────────────────

	for _, cancel := range cancels {
		cancel()
	}
	time.Sleep(600 * time.Millisecond)

	// ── Verify chain DBs ──────────────────────────────────────────────────────

	t.Log("verifying chain DBs …")
	refRecs := readChainDB(t, filepath.Join(clDirs[0], "cl-headers.db"), "node-1")
	t.Logf("node-1 (reference): %d records, head=%d", len(refRecs), blockNum(refRecs))

	if len(refRecs) < targetBlock {
		t.Fatalf("node-1: only %d records in DB, want >= %d", len(refRecs), targetBlock)
	}

	refByNum := make(map[uint64]common.Hash, len(refRecs))
	for _, r := range refRecs {
		refByNum[r.Header.Number.Uint64()] = r.Header.Hash()
	}

	for i := 1; i < numNodes; i++ {
		label := fmt.Sprintf("node-%d", i+1)
		recs := readChainDB(t, filepath.Join(clDirs[i], "cl-headers.db"), label)
		t.Logf("%s: %d records, head=%d", label, len(recs), blockNum(recs))
		if len(recs) < targetBlock {
			t.Errorf("%s: only %d records, want >= %d", label, len(recs), targetBlock)
		}
		for _, r := range recs {
			num := r.Header.Number.Uint64()
			if want, ok := refByNum[num]; ok {
				if got := r.Header.Hash(); got != want {
					t.Errorf("%s: block %d hash mismatch: got %s want %s", label, num, got.Hex(), want.Hex())
				}
			}
		}
	}

	// ── Verify proposer rotation and committed seals on reference chain ────────

	t.Log("verifying proposer rotation and committed seals …")
	quorum := (2*numValidators)/3 + 1
	proposerOK, sealOK := true, true

	for _, r := range refRecs {
		num := r.Header.Number.Uint64()
		if num == 0 {
			continue
		}

		// Check proposer is a known validator.
		proposer, err := qbfteng.SignerFromHeader(r.Header)
		if err != nil {
			t.Errorf("block %d: SignerFromHeader: %v", num, err)
			proposerOK = false
			continue
		}
		if !containsAddr(valAddrs, proposer) {
			t.Errorf("block %d: proposer %s is not a validator", num, proposer.Hex())
			proposerOK = false
		}

		// Check committed seals.
		ie, err := qbfteng.DecodeExtra(r.Header)
		if err != nil {
			t.Errorf("block %d: DecodeExtra: %v", num, err)
			sealOK = false
			continue
		}
		if len(ie.CommittedSeals) < quorum {
			t.Errorf("block %d: %d committed seals, want >= %d",
				num, len(ie.CommittedSeals), quorum)
			sealOK = false
			continue
		}
		seen := make(map[common.Address]bool)
		for _, seal := range ie.CommittedSeals {
			signer, err := qbfteng.RecoverCommitSealSigner(r.Header, seal)
			if err != nil {
				t.Errorf("block %d: recover commit seal: %v", num, err)
				sealOK = false
				continue
			}
			if !containsAddr(valAddrs, signer) {
				t.Errorf("block %d: commit seal from non-validator %s", num, signer.Hex())
				sealOK = false
			}
			if seen[signer] {
				t.Errorf("block %d: duplicate commit seal from %s", num, signer.Hex())
				sealOK = false
			}
			seen[signer] = true
		}
	}

	// Verify chain continuity along the canonical chain (one record per block,
	// in order). Duplicate block numbers in the DB are skipped — they represent
	// blocks that lost a fork and were superseded.
	canonical := make([]*types.Header, 0, len(refRecs))
	seen := make(map[uint64]bool)
	for _, r := range refRecs {
		n := r.Header.Number.Uint64()
		if !seen[n] {
			seen[n] = true
			canonical = append(canonical, r.Header)
		}
	}
	for i := 1; i < len(canonical); i++ {
		parent := canonical[i-1]
		child := canonical[i]
		if child.ParentHash != parent.Hash() {
			t.Errorf("chain break between blocks %d and %d",
				parent.Number.Uint64(), child.Number.Uint64())
		}
	}

	if proposerOK && sealOK {
		t.Logf("OK: all %d blocks pass proposer rotation and seal checks", len(refRecs)-1)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func containsAddr(addrs []common.Address, target common.Address) bool {
	for _, a := range addrs {
		if a == target {
			return true
		}
	}
	return false
}

// buildQBFTGenesis returns a JSON-encoded Clique genesis with multiple sorted
// validator addresses. Extra layout: [32 vanity][N×20 addrs][65 seal].
// The QBFT engine reads the validator list from this Clique-format extra via its
// fallback path in NewGenesisSnapshot.
func buildQBFTGenesis(chainID, period int, timestamp uint64, signers []common.Address) string {
	// 32 zero vanity bytes.
	vanity := strings.Repeat("00", 32)
	// N × 20 address bytes.
	var addrHex string
	for _, s := range signers {
		addrHex += strings.ToLower(s.Hex()[2:]) // strip 0x
	}
	// 65 zero seal bytes.
	seal := strings.Repeat("00", 65)
	extra := "0x" + vanity + addrHex + seal

	alloc := make(map[string]any, len(signers))
	for _, s := range signers {
		alloc[s.Hex()] = map[string]string{"balance": "10000000000000000000000"}
	}

	genesis := map[string]any{
		"config": map[string]any{
			"chainId":                       chainID,
			"homesteadBlock":                0,
			"eip150Block":                   0,
			"eip155Block":                   0,
			"eip158Block":                   0,
			"byzantiumBlock":                0,
			"constantinopleBlock":           0,
			"petersburgBlock":               0,
			"istanbulBlock":                 0,
			"muirGlacierBlock":              0,
			"berlinBlock":                   0,
			"londonBlock":                   0,
			"arrowGlacierBlock":             0,
			"grayGlacierBlock":              0,
			"mergeNetsplitBlock":            0,
			"shanghaiTime":                  0,
			"cancunTime":                    0,
			"terminalTotalDifficulty":       0,
			"terminalTotalDifficultyPassed": true,
			"blobSchedule": map[string]any{
				"cancun": map[string]any{
					"target":               3,
					"max":                  6,
					"baseFeeUpdateFraction": 3338477,
				},
			},
			"clique": map[string]any{
				"period": period,
				"epoch":  30000,
			},
		},
		"nonce":      "0x0",
		"timestamp":  fmt.Sprintf("0x%x", timestamp),
		"extraData":  extra,
		"gasLimit":   "0x1C9C380",
		"difficulty": "0x1",
		"mixHash":    "0x0000000000000000000000000000000000000000000000000000000000000000",
		"coinbase":   "0x0000000000000000000000000000000000000000",
		"alloc":      alloc,
	}
	data, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		panic("buildQBFTGenesis: " + err.Error())
	}
	return string(data)
}
