// Command keygen generates all keys, genesis block, and configuration files
// needed to run the four-node Clique PoA demo. It writes everything under the
// output directory supplied as its first argument.
//
// Outputs written:
//
//	genesis.json               – Clique genesis block (chainId 12345, period 5s)
//	signer-{1,2,3}.hex         – ECDSA private keys for the three block-producing nodes
//	jwt-{1,2,3,4}.hex          – JWT secrets shared between each Geth/clique-node pair
//	geth-{1,2,3,4}/nodekey     – secp256k1 keys used for Geth devp2p identity
//	geth-{1,2,3,4}/static-nodes.json – devp2p static peer lists
//	config/clique-{1,2,3,4}.toml     – clique-node configuration files
//	info.txt                   – human-readable summary (signer addresses, enodes)
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: keygen <output-dir>")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "keygen: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	for _, dir := range []string{
		out,
		filepath.Join(out, "config"),
		filepath.Join(out, "geth-1"),
		filepath.Join(out, "geth-2"),
		filepath.Join(out, "geth-3"),
		filepath.Join(out, "geth-4"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// ── Signer keys (3 block producers) ─────────────────────────────────────

	signerAddrs := make([]string, 3) // checksummed 0x-prefixed addresses
	for i := range 3 {
		key, err := gethcrypto.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate signer key %d: %w", i+1, err)
		}
		signerAddrs[i] = gethcrypto.PubkeyToAddress(key.PublicKey).Hex()
		keyHex := hex.EncodeToString(gethcrypto.FromECDSA(key))
		if err := write(filepath.Join(out, fmt.Sprintf("signer-%d.hex", i+1)), keyHex+"\n"); err != nil {
			return err
		}
	}

	// ── Geth devp2p nodekeys + enode URLs ────────────────────────────────────

	enodes := make([]string, 4)
	for i := range 4 {
		key, err := gethcrypto.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate nodekey %d: %w", i+1, err)
		}
		// nodekey file: raw hex, no newline (Geth reads it as-is)
		nodeKeyHex := hex.EncodeToString(gethcrypto.FromECDSA(key))
		if err := write(filepath.Join(out, fmt.Sprintf("geth-%d", i+1), "nodekey"), nodeKeyHex); err != nil {
			return err
		}
		// enode = enode://<64-byte-pubkey-hex>@<container-name>:30303
		pubHex := hex.EncodeToString(gethcrypto.FromECDSAPub(&key.PublicKey)[1:])
		enodes[i] = fmt.Sprintf("enode://%s@geth-%d:30303", pubHex, i+1)
	}

	// static-nodes.json: each geth connects to all others
	for i := range 4 {
		var peers []string
		for j, e := range enodes {
			if j != i {
				peers = append(peers, e)
			}
		}
		data, err := json.MarshalIndent(peers, "", "  ")
		if err != nil {
			return err
		}
		dest := filepath.Join(out, fmt.Sprintf("geth-%d", i+1), "static-nodes.json")
		if err := write(dest, string(data)+"\n"); err != nil {
			return err
		}
	}

	// ── JWT secrets ──────────────────────────────────────────────────────────

	for i := range 4 {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return fmt.Errorf("generate JWT secret %d: %w", i+1, err)
		}
		if err := write(filepath.Join(out, fmt.Sprintf("jwt-%d.hex", i+1)), hex.EncodeToString(secret)+"\n"); err != nil {
			return err
		}
	}

	// ── Genesis block ────────────────────────────────────────────────────────

	// extraData: 32-byte vanity | sorted signer addresses (20 bytes each) | 65-byte seal
	lowerAddrs := make([]string, 3)
	for i, a := range signerAddrs {
		lowerAddrs[i] = strings.ToLower(a[2:]) // strip 0x, lowercase
	}
	sort.Strings(lowerAddrs)

	extra := "0x" + strings.Repeat("00", 32)
	for _, a := range lowerAddrs {
		extra += a
	}
	extra += strings.Repeat("00", 65)

	alloc := map[string]interface{}{}
	for _, addr := range signerAddrs {
		alloc[addr] = map[string]string{"balance": "10000000000000000000000"} // 10 000 ETH each
	}

	genesis := map[string]interface{}{
		"config": map[string]interface{}{
			"chainId":                       12345,
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
			// blobSchedule is required by Geth v1.15+ when cancunTime is set.
			// Values are the standard EIP-4844 Cancun parameters.
			"blobSchedule": map[string]interface{}{
				"cancun": map[string]interface{}{
					"target":               3,
					"max":                  6,
					"baseFeeUpdateFraction": 3338477,
				},
			},
			"clique": map[string]interface{}{
				"period": 5,
				"epoch":  30000,
			},
		},
		"nonce":      "0x0",
		"timestamp":  "0x0",
		"extraData":  extra,
		"gasLimit":   "0x1C9C380",
		"difficulty": "0x1",
		"mixHash":    "0x0000000000000000000000000000000000000000000000000000000000000000",
		"coinbase":   "0x0000000000000000000000000000000000000000",
		"alloc":      alloc,
	}
	genesisJSON, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		return err
	}
	if err := write(filepath.Join(out, "genesis.json"), string(genesisJSON)+"\n"); err != nil {
		return err
	}

	// ── clique-node config files ──────────────────────────────────────────────

	for i := range 4 {
		num := i + 1
		var signerLine string
		if i < 3 {
			signerLine = fmt.Sprintf(`signer_key_path = "/generated/signer-%d.hex"`, num)
		} else {
			signerLine = `# no signer_key_path — running in follower / observer mode`
		}
		cfg := fmt.Sprintf(`[node]
network_id = 12345

[engine]
url            = "http://geth-%d:8551"
jwt_secret_path = "/generated/jwt-%d.hex"
el_rpc_url     = "http://geth-%d:8545"
dial_timeout   = "30s"
call_timeout   = "10s"

[p2p]
listen_addr  = "/ip4/0.0.0.0/tcp/9000"
boot_nodes   = []
max_peers    = 50
enable_mdns  = true

[rpc]
listen_addr   = "0.0.0.0:5052"
read_timeout  = "10s"
write_timeout = "10s"

[clique]
%s
period = 5
epoch  = 30000

[logging]
level  = "debug"
format = "json"
`, num, num, num, signerLine)
		if err := write(filepath.Join(out, "config", fmt.Sprintf("clique-%d.toml", num)), cfg); err != nil {
			return err
		}
	}

	// ── human-readable summary ────────────────────────────────────────────────

	var sb strings.Builder
	sb.WriteString("# Demo network — generated keys\n")
	sb.WriteString("# DO NOT use these keys on any real network.\n\n")
	sb.WriteString("## Signer addresses (funded with 10 000 ETH each)\n\n")
	for i, addr := range signerAddrs {
		sb.WriteString(fmt.Sprintf("  Node %d: %s\n", i+1, addr))
	}
	sb.WriteString("\n## Geth devp2p enode URLs\n\n")
	for i, e := range enodes {
		sb.WriteString(fmt.Sprintf("  geth-%d: %s\n", i+1, e))
	}
	sb.WriteString("\n## Chain\n\n")
	sb.WriteString("  chainId  : 12345\n")
	sb.WriteString("  period   : 5 s\n")
	sb.WriteString("  epoch    : 30000 blocks\n")
	if err := write(filepath.Join(out, "info.txt"), sb.String()); err != nil {
		return err
	}

	fmt.Printf("Generated keys and configs → %s\n", out)
	fmt.Println("Signer addresses:")
	for i, addr := range signerAddrs {
		fmt.Printf("  node-%d: %s\n", i+1, addr)
	}
	return nil
}

func write(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
