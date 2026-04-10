# Four-Node Clique PoA Demo

This demo spins up a complete, self-contained Clique PoA network using Docker:

| Container | Role | Ports (host) |
|---|---|---|
| `geth-1` | Execution client (primary) | `8545` JSON-RPC, `8551` Engine API |
| `geth-2` | Execution client | `8546` JSON-RPC |
| `geth-3` | Execution client | `8547` JSON-RPC |
| `geth-4` | Execution client (observer) | `8548` JSON-RPC |
| `clique-1` | Consensus client — **signer** | `5052` RPC, `9001` P2P |
| `clique-2` | Consensus client — **signer** | `5053` RPC, `9002` P2P |
| `clique-3` | Consensus client — **signer** | `5054` RPC, `9003` P2P |
| `clique-4` | Consensus client — **observer** | `5055` RPC, `9004` P2P |

`clique-4` has no signer key configured. It follows the chain, validates incoming blocks, and updates its paired `geth-4` via the Engine API — demonstrating the follower mode.

## Prerequisites

- [Docker Desktop](https://docs.docker.com/get-docker/) (Mac/Windows) or Docker Engine + Compose plugin (Linux)
- Docker Compose v2 (`docker compose version`)

No other tools are required. The `setup.sh` script builds all images and generates all keys inside Docker.

## Starting the Demo

```bash
cd scripts/demo
./setup.sh
```

The script:

1. Builds the `clique-node:demo` image from the project root
2. Builds a `clique-node-keygen` image that generates keys and configuration
3. Generates ECDSA signer keys, Geth devp2p nodekeys, JWT secrets, and a shared genesis block
4. Initialises each Geth data directory (`data/geth-{1,2,3,4}/`) with the genesis block
5. Places the pre-generated `nodekey` and `static-nodes.json` into each Geth datadir so the four Geth instances peer with each other via devp2p
6. Starts all eight containers via Docker Compose

The setup is **idempotent** — running `./setup.sh` again after the first start skips key generation and Geth initialisation. Only new containers will be started if any stopped.

Allow about 10–15 seconds for the Geth healthchecks to pass and for the first block to be produced.

## Observing Block Production

### Watch consensus client logs

```bash
./setup.sh --logs
```

Look for lines like:
```
{"level":"info","component":"node","number":1,"hash":"0x...","signer":"0x...","message":"block produced and imported"}
{"level":"info","component":"node","number":1,"hash":"0x...","message":"new canonical head (from P2P)"}
```

To watch a single node:
```bash
docker compose -p clique-demo logs -f clique-1
```

### Poll the chain head via JSON-RPC

```bash
# Current block number on geth-1
curl -s -X POST http://localhost:8545 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  | jq .

# Fetch the latest block with full transaction details
curl -s -X POST http://localhost:8545 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",true],"id":1}' \
  | jq .
```

### Query the consensus client RPC API

```bash
# Current head block (number, hash, signer address)
curl -s http://localhost:5052/clique/v1/head | jq .

# Current authorized signer set
curl -s http://localhost:5052/clique/v1/validators | jq .

# Number of peers seen by clique-1
curl -s http://localhost:5052/eth/v1/node/peers | jq .meta.count

# Node identity (libp2p peer ID and multiaddrs)
curl -s http://localhost:5052/eth/v1/node/identity | jq .

# Observer node (clique-4) — same head, no "signer" field
curl -s http://localhost:5055/clique/v1/head | jq .
```

### Watch block numbers advance across all nodes

```bash
watch -n 2 '
  for port in 8545 8546 8547 8548; do
    block=$(curl -s -X POST http://localhost:$port \
      -H "Content-Type: application/json" \
      -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}" \
      | jq -r .result)
    printf "geth on :%s  block %d\n" "$port" "$((block))"
  done
'
```

### Verify Clique turn order

Blocks alternate between the three signers in round-robin order. Each signer produces one block per three-block rotation (period = 5 s, so one block every ~15 s per signer). Check the `miner` field of each block — it should cycle through the three signer addresses:

```bash
for n in 1 2 3 4 5 6; do
  curl -s -X POST http://localhost:8545 \
    -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$(printf '0x%x' $n)\",false],\"id\":1}" \
    | jq -r '"\(.result.number) miner=\(.result.miner)"'
done
```

## Submitting Transactions

### Option 1 — MetaMask

1. Open MetaMask → **Add network manually**
2. Fill in:
   - **Network name**: Clique Demo
   - **RPC URL**: `http://localhost:8545`
   - **Chain ID**: `12345`
   - **Currency symbol**: ETH
3. Import a signer account using the private key from `generated/signer-1.hex`
4. Send ETH or deploy contracts normally

### Option 2 — cast (Foundry)

If you have [Foundry](https://book.getfoundry.sh/getting-started/installation) installed:

```bash
# Read signer private key
SIGNER_KEY=$(cat generated/signer-1.hex)
SIGNER_ADDR=$(cast wallet address --private-key 0x${SIGNER_KEY})

# Check balance
cast balance --rpc-url http://localhost:8545 ${SIGNER_ADDR}

# Send 1 ETH to a random address
cast send \
  --rpc-url http://localhost:8545 \
  --private-key 0x${SIGNER_KEY} \
  0x000000000000000000000000000000000000dEaD \
  --value 1ether

# Wait for the next block and verify inclusion
cast tx --rpc-url http://localhost:8545 <tx-hash>
```

### Option 3 — raw JSON-RPC (eth_sendRawTransaction)

Sign a transaction offline and submit it:

```bash
# Using cast to sign
SIGNED=$(cast mktx \
  --rpc-url http://localhost:8545 \
  --private-key 0x$(cat generated/signer-1.hex) \
  0x000000000000000000000000000000000000dEaD \
  --value 0.1ether)

curl -s -X POST http://localhost:8545 \
  -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_sendRawTransaction\",\"params\":[\"${SIGNED}\"],\"id\":1}" \
  | jq .
```

## Voting to Add or Remove a Signer

The consensus client exposes a voting endpoint. A pending vote is embedded in the next block the signer produces.

```bash
# Vote to authorize a new signer (from clique-1's turn)
curl -s -X POST http://localhost:5052/clique/v1/vote \
  -H 'Content-Type: application/json' \
  -d '{"address":"0xNewSignerAddressHere","authorize":true}' \
  | jq .

# Vote to remove a signer
curl -s -X POST http://localhost:5052/clique/v1/vote \
  -H 'Content-Type: application/json' \
  -d '{"address":"0xSignerToRemove","authorize":false}' \
  | jq .

# Check in-progress votes
curl -s http://localhost:5052/clique/v1/votes | jq .
```

A vote is active for one epoch (30 000 blocks). Majority (more than half the signers) must vote to change the signer set.

## Generated Files

After running `./setup.sh`, the `generated/` directory contains all secrets for the demo network. **Do not commit or share these files.** They are regenerated each time you run `./setup.sh --reset`.

```
generated/
├── info.txt              — signer addresses and enode URLs (human readable)
├── genesis.json          — Clique genesis block (chainId 12345)
├── signer-{1,2,3}.hex   — ECDSA private keys for block producers
├── jwt-{1,2,3,4}.hex    — JWT secrets for Engine API auth
├── geth-{1,2,3,4}/
│   ├── nodekey           — Geth devp2p identity key
│   └── static-nodes.json — devp2p static peer list
└── config/
    └── clique-{1,2,3,4}.toml — clique-node config files
```

## Stopping and Resetting

```bash
# Stop all containers (keeps data for next start)
./setup.sh --stop

# Full reset: remove containers, generated keys, and chain data
./setup.sh --reset
```

## Architecture Notes

- **Block period**: 5 seconds (configurable via `clique.period` in genesis and node config)
- **Epoch**: 30 000 blocks (vote reset and signer-list checkpoint)
- **Devp2p peering**: the four Geth instances connect to each other using `static-nodes.json` populated with pre-generated devp2p node keys; discovery is disabled (`--nodiscover`) to keep the network self-contained
- **libp2p peering**: the four consensus clients discover each other via mDNS within the Docker bridge network; no explicit bootstrap addresses are needed
- **Block propagation**: a produced block is published via Gossipsub by the producing consensus client, received by the other three, and forwarded to their respective Geth instances via `engine_forkchoiceUpdatedV3`
- **Transaction propagation**: transactions submitted to any Geth node travel across devp2p to all other Geth nodes; they are available to be included by whichever signer produces the next block

## Troubleshooting

**Blocks stop being produced**

Check that the consensus clients are connected to their Geth instances:
```bash
curl -s http://localhost:5052/eth/v1/node/health
```
A `200` response means the node is healthy. A `503` means it has no peers or the chain head is unknown.

**Consensus clients can't find each other**

mDNS multicast works within a single Docker bridge network. If you are on a platform where multicast is not forwarded (rare with Docker Desktop), restart the clique-node containers:
```bash
docker compose -p clique-demo restart clique-1 clique-2 clique-3 clique-4
```

**Geth nodes are not peering**

Verify that the `static-nodes.json` files were written before Geth started:
```bash
cat data/geth-1/geth/static-nodes.json
```
If the file is missing, run `./setup.sh --reset` to start over.
