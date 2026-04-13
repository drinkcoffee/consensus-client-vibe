//go:build integration

// Package integration_test contains end-to-end tests that start real geth
// instances and run clique-node in process.
//
// geth is built automatically from the pinned submodule on first run.
// See third_party/go-ethereum and scripts/build-geth.sh.
//
// Run with:
//
//	go test -v -tags integration -timeout 300s ./test/integration/
package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	"github.com/peterrobinson/consensus-client-vibe/internal/forkchoice"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
	"github.com/peterrobinson/consensus-client-vibe/internal/node"
)

func init() {
	log.Init("info", "pretty")
}

// TestSync_FollowerCatchesUp exercises the full sync path end-to-end:
//
//  1. A single-validator Clique network is started (node-1 + geth-1).
//  2. After node-1 has produced 20 blocks, a follower (node-2 + geth-2) joins.
//  3. node-2 has no signer key; it discovers node-1 as a boot node and triggers
//     the /clique/sync/1 protocol to download the missing blocks.
//  4. The test waits until node-1 reaches block 40 and node-2 is caught up.
//  5. Both nodes are stopped and their on-disk chain DBs are inspected to verify
//     that node-2 holds the same canonical chain as node-1.
func TestSync_FollowerCatchesUp(t *testing.T) {
	gethBin := gethBinary(t)

	dir := t.TempDir()

	// ── Signer key ──────────────────────────────────────────────────────────

	signerKey, err := gethcrypto.GenerateKey()
	if err != nil {
		t.Fatal("generate signer key:", err)
	}
	signerAddr := gethcrypto.PubkeyToAddress(signerKey.PublicKey)
	signerKeyPath := filepath.Join(dir, "signer.hex")
	writeFile(t, signerKeyPath, hex.EncodeToString(gethcrypto.FromECDSA(signerKey))+"\n")

	// ── JWT secrets (one per geth instance) ─────────────────────────────────

	jwt1Path := writeJWT(t, filepath.Join(dir, "jwt-1.hex"))
	jwt2Path := writeJWT(t, filepath.Join(dir, "jwt-2.hex"))

	// ── Genesis ─────────────────────────────────────────────────────────────
	//
	// period=1 s keeps the test fast.  genesis.Time is set one second in the
	// past so the first block fires immediately; subsequent blocks are spaced
	// ~1 second apart.

	const (
		chainID = 54321
		period  = 1
	)
	genesisTime := uint64(time.Now().Unix()) - 1

	genesisPath := filepath.Join(dir, "genesis.json")
	writeFile(t, genesisPath, buildGenesis(chainID, period, genesisTime, signerAddr))

	// ── geth-1 (validator's execution client) ───────────────────────────────

	geth1Dir := filepath.Join(dir, "geth-1")
	initGeth(t, gethBin, geth1Dir, genesisPath)

	enginePort1, httpPort1, p2pPort1 := freePort(t), freePort(t), freePort(t)
	geth1 := startGeth(t, gethBin, geth1Dir, chainID, enginePort1, httpPort1, p2pPort1, jwt1Path,
		filepath.Join(dir, "geth-1.log"))
	t.Cleanup(func() { _ = geth1.Process.Kill(); _ = geth1.Wait() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d", httpPort1), 30*time.Second,
		filepath.Join(dir, "geth-1.log"))

	// ── node-1: single-validator ─────────────────────────────────────────────

	clDir1 := filepath.Join(dir, "cl-1")
	cfg1 := config.DefaultConfig()
	cfg1.Node.NetworkID = chainID
	cfg1.Node.DataDir = clDir1
	cfg1.Engine.URL = fmt.Sprintf("http://127.0.0.1:%d", enginePort1)
	cfg1.Engine.JWTSecretPath = jwt1Path
	cfg1.Engine.ELRPCUrl = fmt.Sprintf("http://127.0.0.1:%d", httpPort1)
	cfg1.P2P.ListenAddr = "/ip4/127.0.0.1/tcp/0"
	cfg1.P2P.MaxPeers = 10
	cfg1.P2P.BootNodes = nil
	cfg1.RPC.ListenAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg1.Clique.SignerKeyPath = signerKeyPath
	cfg1.Clique.Period = period
	cfg1.Clique.Epoch = 30000
	node1, err := node.New(cfg1)
	if err != nil {
		t.Fatal("create node-1:", err)
	}

	// Capture the P2P address while the libp2p host is bound but not yet
	// started — the listen socket is held open from New() onwards.
	node1Addr := node1.P2PAddr()
	t.Logf("node-1 P2P: %s", node1Addr)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		if err := node1.Start(ctx1); err != nil {
			t.Logf("node-1 exited: %v", err)
		}
	}()

	// ── Wait for 20 blocks ───────────────────────────────────────────────────

	t.Log("waiting for node-1 to reach block 20 …")
	waitForBlock(t, node1, 20, 90*time.Second)
	t.Logf("node-1 head=%d — starting follower", node1.HeadNumber())

	// ── geth-2 (follower's execution client) ────────────────────────────────

	geth2Dir := filepath.Join(dir, "geth-2")
	initGeth(t, gethBin, geth2Dir, genesisPath)

	enginePort2, httpPort2, p2pPort2 := freePort(t), freePort(t), freePort(t)
	geth2 := startGeth(t, gethBin, geth2Dir, chainID, enginePort2, httpPort2, p2pPort2, jwt2Path,
		filepath.Join(dir, "geth-2.log"))
	t.Cleanup(func() { _ = geth2.Process.Kill(); _ = geth2.Wait() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d", httpPort2), 30*time.Second,
		filepath.Join(dir, "geth-2.log"))

	// ── node-2: follower / observer ──────────────────────────────────────────

	clDir2 := filepath.Join(dir, "cl-2")
	cfg2 := config.DefaultConfig()
	cfg2.Node.NetworkID = chainID
	cfg2.Node.DataDir = clDir2
	cfg2.Engine.URL = fmt.Sprintf("http://127.0.0.1:%d", enginePort2)
	cfg2.Engine.JWTSecretPath = jwt2Path
	cfg2.Engine.ELRPCUrl = fmt.Sprintf("http://127.0.0.1:%d", httpPort2)
	cfg2.P2P.ListenAddr = "/ip4/127.0.0.1/tcp/0"
	cfg2.P2P.MaxPeers = 10
	cfg2.P2P.BootNodes = []string{node1Addr}
	cfg2.RPC.ListenAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg2.Clique.SignerKeyPath = "" // no signer key → follower
	cfg2.Clique.Period = period
	cfg2.Clique.Epoch = 30000
	node2, err := node.New(cfg2)
	if err != nil {
		t.Fatal("create node-2:", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() {
		if err := node2.Start(ctx2); err != nil {
			t.Logf("node-2 exited: %v", err)
		}
	}()

	// ── Wait for validator to reach block 40 ────────────────────────────────

	t.Log("waiting for node-1 to reach block 40 …")
	waitForBlock(t, node1, 40, 90*time.Second)
	t.Logf("node-1 head=%d", node1.HeadNumber())

	// ── Wait for follower to sync up ─────────────────────────────────────────

	t.Log("waiting for node-2 to sync to block 40 …")
	waitForBlock(t, node2, 40, 60*time.Second)
	t.Logf("node-2 head=%d", node2.HeadNumber())

	// ── Graceful shutdown ────────────────────────────────────────────────────

	cancel1()
	cancel2()
	time.Sleep(600 * time.Millisecond) // let shutdown complete before reading DBs

	// ── Verify on-disk chain DBs ─────────────────────────────────────────────

	t.Log("verifying on-disk chain DB integrity …")
	verifyChainSync(t, clDir1, clDir2, 40)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// writeFile writes content to path, creating parent directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal("mkdir:", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal("writeFile:", err)
	}
}

// writeJWT generates a random 32-byte JWT secret and writes it as hex to path.
func writeJWT(t *testing.T, path string) string {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("generate JWT secret:", err)
	}
	writeFile(t, path, hex.EncodeToString(secret)+"\n")
	return path
}

// buildGenesis returns a JSON-encoded Clique genesis block with a single signer.
//
// extraData layout: 32-byte vanity | 20-byte signer address | 65-byte seal
func buildGenesis(chainID, period int, timestamp uint64, signer common.Address) string {
	extra := "0x" +
		strings.Repeat("00", 32) +
		strings.ToLower(signer.Hex()[2:]) + // strip 0x prefix, lowercase
		strings.Repeat("00", 65)

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
		"alloc": map[string]any{
			signer.Hex(): map[string]string{"balance": "10000000000000000000000"},
		},
	}
	data, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		panic("buildGenesis: " + err.Error())
	}
	return string(data)
}

// freePort finds an available TCP port on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("find free port:", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// initGeth runs "geth --datadir=<dir> init <genesis>" to initialise the chain.
func initGeth(t *testing.T, bin, datadir, genesisPath string) {
	t.Helper()
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		t.Fatal("mkdir geth datadir:", err)
	}
	out, err := exec.Command(bin, "--datadir="+datadir, "init", genesisPath).CombinedOutput()
	if err != nil {
		t.Fatalf("geth init: %v\n%s", err, out)
	}
}

// startGeth launches a geth process and redirects all output to logPath.
// The caller is responsible for killing the process (typically via t.Cleanup).
func startGeth(t *testing.T, bin, datadir string, chainID, enginePort, httpPort, p2pPort int,
	jwtPath, logPath string) *exec.Cmd {
	t.Helper()

	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal("create geth log:", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.Command(bin,
		"--datadir="+datadir,
		fmt.Sprintf("--networkid=%d", chainID),
		"--authrpc.addr=127.0.0.1",
		fmt.Sprintf("--authrpc.port=%d", enginePort),
		"--authrpc.jwtsecret="+jwtPath,
		"--authrpc.vhosts=*",
		"--http",
		"--http.addr=127.0.0.1",
		fmt.Sprintf("--http.port=%d", httpPort),
		"--http.api=eth,net,web3",
		"--http.vhosts=*",
		fmt.Sprintf("--port=%d", p2pPort),
		"--ipcdisable",
		"--nodiscover",
		"--maxpeers=0",
		"--syncmode=full",
		"--gcmode=archive",
		"--verbosity=3",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		t.Fatal("start geth:", err)
	}
	t.Logf("geth started (pid %d) logs → %s", cmd.Process.Pid, logPath)
	return cmd
}

// waitForHTTP polls url with an eth_blockNumber JSON-RPC call until geth's
// HTTP server is accepting requests or timeout elapses.
// logPath (optional) is the geth log file; its tail is printed on failure.
func waitForHTTP(t *testing.T, url string, timeout time.Duration, logPath ...string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	const payload = `{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`
	deadline := time.Now().Add(timeout)
	var lastErr string
	for time.Now().Before(deadline) {
		resp, err := client.Post(url, "application/json", strings.NewReader(payload))
		if err != nil {
			lastErr = err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// On failure, dump the last few lines of the geth log if available.
	if len(logPath) > 0 && logPath[0] != "" {
		if data, err := os.ReadFile(logPath[0]); err == nil {
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			tail := lines
			if len(tail) > 40 {
				tail = tail[len(tail)-40:]
			}
			t.Logf("=== geth log tail (%s) ===\n%s", logPath[0], strings.Join(tail, "\n"))
		}
	}
	t.Fatalf("geth at %s not ready after %s (last error: %s)", url, timeout, lastErr)
}

// waitForBlock polls n.HeadNumber() every 500 ms until it reaches target or
// timeout elapses, logging progress every time the head advances.
func waitForBlock(t *testing.T, n *node.Node, target uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var logged uint64
	for time.Now().Before(deadline) {
		cur := n.HeadNumber()
		if cur >= target {
			return
		}
		if cur > logged {
			t.Logf("  head=%d, waiting for %d", cur, target)
			logged = cur
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for block %d — head=%d", target, n.HeadNumber())
}

// verifyChainSync opens the chain DBs written by both nodes after shutdown and
// checks that node-2's canonical chain matches node-1's for all recorded blocks.
//
// It asserts:
//   - Both DBs contain at least minBlocks records.
//   - All records in node-2's DB appear in node-1's DB with the same header hash.
//   - node-2's records form an unbroken chain (ParentHash links are correct).
func verifyChainSync(t *testing.T, clDir1, clDir2 string, minBlocks int) {
	t.Helper()

	recs1 := readChainDB(t, filepath.Join(clDir1, "cl-headers.db"), "node-1")
	recs2 := readChainDB(t, filepath.Join(clDir2, "cl-headers.db"), "node-2")

	t.Logf("node-1 chain DB: %d records (head block %d)", len(recs1), blockNum(recs1))
	t.Logf("node-2 chain DB: %d records (head block %d)", len(recs2), blockNum(recs2))

	if len(recs1) < minBlocks {
		t.Errorf("node-1 chain DB: got %d records, want >= %d", len(recs1), minBlocks)
	}
	if len(recs2) < minBlocks {
		t.Errorf("node-2 chain DB: got %d records, want >= %d", len(recs2), minBlocks)
	}

	// Build a number→hash index from node-1's chain.
	index1 := make(map[uint64]common.Hash, len(recs1))
	for _, r := range recs1 {
		index1[r.Header.Number.Uint64()] = r.Header.Hash()
	}

	// Verify every block node-2 persisted matches node-1's canonical chain.
	mismatches := 0
	for _, r := range recs2 {
		num := r.Header.Number.Uint64()
		want, ok := index1[num]
		if !ok {
			t.Errorf("block %d present in node-2 DB but absent from node-1 DB", num)
			mismatches++
			continue
		}
		if got := r.Header.Hash(); got != want {
			t.Errorf("block %d hash mismatch: node-2=%s node-1=%s",
				num, got.Hex(), want.Hex())
			mismatches++
		}
	}

	// Verify internal chain continuity in node-2's DB.
	for i := 1; i < len(recs2); i++ {
		parent := recs2[i-1].Header
		child := recs2[i].Header
		if child.ParentHash != parent.Hash() {
			t.Errorf("chain break at block %d: ParentHash=%s want=%s",
				child.Number.Uint64(), child.ParentHash.Hex(), parent.Hash().Hex())
		}
	}

	if mismatches == 0 && len(recs2) >= minBlocks {
		t.Logf("OK: node-2 DB matches node-1 for all %d verified blocks", len(recs2))
	}
}

// readChainDB opens and reads all records from a chain DB file.
func readChainDB(t *testing.T, path, label string) []forkchoice.DBRecord {
	t.Helper()
	db, err := forkchoice.OpenChainDB(path)
	if err != nil {
		t.Fatalf("open %s chain DB (%s): %v", label, path, err)
	}
	defer db.Close()

	recs, err := db.ReadAll()
	if err != nil {
		t.Fatalf("read %s chain DB: %v", label, err)
	}
	return recs
}

// blockNum returns the block number of the last record, or 0 if empty.
func blockNum(recs []forkchoice.DBRecord) uint64 {
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].Header.Number.Uint64()
}

// ── submodule geth ────────────────────────────────────────────────────────────

// gethBinary returns the path to the geth binary built from the pinned
// go-ethereum submodule at third_party/go-ethereum.
//
// On first call it checks whether the binary already exists.  If not, it
// initialises the submodule (populating the source tree from the network) and
// builds geth with "go build ./cmd/geth".  Subsequent calls are instant.
//
// The binary is placed at:
//
//	<repo-root>/third_party/go-ethereum/build/bin/geth
func gethBinary(t *testing.T) string {
	t.Helper()

	root := repoRoot(t)
	submoduleDir := filepath.Join(root, "third_party", "go-ethereum")
	binPath := filepath.Join(submoduleDir, "build", "bin", "geth")

	// Fast path: binary already built.
	if _, err := os.Stat(binPath); err == nil {
		return binPath
	}

	// Ensure the submodule source tree is present.
	if _, err := os.Stat(filepath.Join(submoduleDir, "go.mod")); err != nil {
		t.Log("initialising go-ethereum submodule (shallow clone, one-time) …")
		cmd := exec.Command("git", "-C", root,
			"submodule", "update", "--init", "--depth=1",
			"third_party/go-ethereum")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git submodule update: %v", err)
		}
	}

	// Build geth.
	t.Log("building geth from submodule (one-time, may take a minute) …")
	if err := os.MkdirAll(filepath.Join(submoduleDir, "build", "bin"), 0o755); err != nil {
		t.Fatal("mkdir build/bin:", err)
	}
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/geth")
	cmd.Dir = submoduleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build geth: %v", err)
	}

	t.Logf("geth built: %s", binPath)
	return binPath
}

// repoRoot walks up from this source file's directory until it finds go.mod,
// then returns that directory as the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	// runtime.Caller(0) gives the absolute path to this source file at compile
	// time, which is reliable even when tests are run from a different cwd.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repoRoot: could not find go.mod walking up from " + filepath.Dir(file))
		}
		dir = parent
	}
}
