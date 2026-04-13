# Integration Tests

End-to-end tests that start real `geth` instances and run `clique-node` in-process.
These tests are gated behind the `integration` build tag so they never run during
normal `go test ./...` invocations.

## geth binary

The tests use the `geth` binary built from the **pinned go-ethereum submodule** at
[`third_party/go-ethereum`](../../third_party/go-ethereum) (v1.17.2, commit
`be4dc0c4`).  You do not need geth installed separately.

**The binary is built automatically on first test run.**  If you prefer to build
it ahead of time:

```bash
# From the repository root:
./scripts/build-geth.sh          # build once (no-op if already built)
./scripts/build-geth.sh --force  # rebuild unconditionally
```

After the first build the binary lives at:

```
third_party/go-ethereum/build/bin/geth
```

### Fresh clone setup

On a fresh clone the submodule directory is empty.  `gethBinary()` in the test
helper will initialise and build it automatically, or you can do it manually:

```bash
git submodule update --init --depth=1 third_party/go-ethereum
./scripts/build-geth.sh
```

## Running

```bash
go test -v -tags integration -timeout 300s ./test/integration/
```

To run a specific test:

```bash
go test -v -tags integration -timeout 300s -run TestSync_FollowerCatchesUp ./test/integration/
```

## Tests

| File | Test | Description |
|---|---|---|
| `sync_test.go` | `TestSync_FollowerCatchesUp` | Starts a single-validator Clique network. After the validator has produced 20 blocks, a follower node joins with no signer key and syncs the chain via the `/clique/sync/1` protocol. The test waits for the validator to reach block 40, then verifies that the follower is fully caught up. On shutdown, both nodes' on-disk chain DBs are opened directly and checked for correctness: the follower must hold ≥ 40 records, every block hash must match the validator's canonical chain, and all parent–child hash links must be intact. |

## What each test exercises

### `TestSync_FollowerCatchesUp`

**Network topology**

```
geth-1 ←──engine API──→ node-1 (validator, signer key)
                              │
                         libp2p /clique/sync/1
                              │
geth-2 ←──engine API──→ node-2 (follower, no key)
```

The two geth instances are isolated (`--nodiscover --maxpeers=0`). The execution
layer knows nothing about the other node — all block delivery happens through the
CL sync and gossip protocols.

**Step-by-step**

1. `gethBinary(t)` resolves the path to the submodule binary, initialising and
   building it on first run.
2. Generate a fresh ECDSA signer key, two JWT secrets, and a Clique genesis block
   (chain ID 54321, 1-second block period, single signer).
3. Initialise and start **geth-1**; wait for its HTTP JSON-RPC to become ready.
4. Create **node-1** wired to geth-1. Capture its libp2p multiaddr immediately
   after construction (the listen socket is bound in `node.New`, before `Start`).
5. Start node-1. It begins producing blocks as the sole in-turn signer.
6. Wait (up to 90 s) for node-1 to reach **block 20**.
7. Initialise and start **geth-2**; wait for its HTTP JSON-RPC to become ready.
8. Create **node-2** wired to geth-2, with node-1's multiaddr as its only boot
   node. node-2 has no signer key and will not produce blocks.
9. Start node-2. On connecting to node-1 the status handshake reveals that node-1's
   head is ahead; node-2 immediately opens a `/clique/sync/1` stream and downloads
   the missing blocks. For each block it calls `engine_newPayloadV3` on geth-2
   before adding the header to its fork-choice store, then sends a final
   `engine_forkchoiceUpdatedV3` once the sync batch is complete.
10. Wait (up to 90 s) for node-1 to reach **block 40**; wait (up to 60 s) for
    node-2 to sync to block 40 via gossip.
11. Cancel both node contexts and allow a brief grace period for clean shutdown.
12. **Verify** by reopening both on-disk chain DBs (`cl-headers.db`) directly:
    - Both DBs contain ≥ 40 records.
    - Every block in node-2's DB has the same header hash as the corresponding
      block in node-1's DB.
    - node-2's records form an unbroken chain (each `ParentHash` matches the
      previous record's hash).

**Logs**

All geth output is redirected to `geth-1.log` and `geth-2.log` inside the
temporary test directory. `t.TempDir()` preserves the directory on test failure,
so these logs are available for debugging. Pass `-v` to see node head-progress
lines printed by `waitForBlock`.
