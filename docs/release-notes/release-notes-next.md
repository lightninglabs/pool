# Next release

## Bug Fixes

* Fix a connection ownership bug where an embedded pool trader server closed
  the shared lnd connection on shutdown. When running as a subserver of
  lightning-terminal, stopping the pool daemon tore down the lnd connection
  owned by the parent process, breaking lnd access for LiT and all other
  integrated subservers. The server now tracks whether it created the
  connection itself and only closes owned connections.

* Disable MPTCP listeners by default, restoring plain TCP for `poold` and
  mitigating the demonstrated CVE-2026-98432 exploit path on vulnerable Linux
  kernels. Existing binaries can apply the same mitigation with
  `GODEBUG=multipathtcp=0` and a restart. A patched kernel is still required to
  fix the underlying vulnerability.

* Fix the `ListLeases` on-chain fee estimation to account for the account
  version at the time of the executed batch rather than always assuming
  P2WSH, so estimates are correct for Taproot accounts (#532, closes #431).

## Breaking Changes

* Migrate exported Bitcoin types and dependencies to the btcd v2 modules.
  Source builds require Go 1.26.8, and L402 token payments require lnd v0.19.0-beta.

## Security

* Bump gRPC to v1.85.0, x/net to v0.62.0, klauspost/compress to v1.18.7,
  moby/sys/user to v0.4.1, opencontainers/runc to v1.3.6, and the
  OpenTelemetry stack to v1.45.0 across the root, `auctioneerrpc`, and
  `poolrpc` modules, clearing all open Dependabot security advisories (#531).

## Maintenance

* Retain the lnd v0.21.4 recovery fixes while migrating to btcd v2.
