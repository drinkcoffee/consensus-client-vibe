# Implementation Plan

**Done:**
- Clique block validation and production
- Engine API integration with any post-Merge-capable EL
- P2P block propagation between Clique consensus nodes
- REST API for monitoring and validator management
- Sync from genesis (start from trusted checkpoint or paired EL)
- Consensus layer blockchain is persisted to disk
- Docker: works as in a Docker image with an example script.



**Out of scope (initially):**
- Not needed: Slashing / equivocation detection
- Not needed: MEV / builder API integration
- Not needed: BLS attestations (not applicable to Clique)

**To Check After Initial Build**
- Clean up logs -> remove my debug log.
- Look at config: does it make sense. Does it use something similar to geth
- Allow multiple consensus algorithms
- Add our custom Clique algorithm.
- Going from pre-merge network. What does that mean?


- Change name to ???EthBftConsensusClient
- Going from pre-merge network.
- Key storage in KMS.
- Check:
  - Buffer overflows in P2P
  - Is Clique implemented correctly?
  - Are the latest versions of third party libraries being used?
  - Do feature comparison between other consensus client to understand differences
