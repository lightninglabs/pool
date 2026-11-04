# Next release

## Bug Fixes

* Fix a connection ownership bug where an embedded pool trader server closed
  the shared lnd connection on shutdown. When running as a subserver of
  lightning-terminal, stopping the pool daemon tore down the lnd connection
  owned by the parent process, breaking lnd access for LiT and all other
  integrated subservers. The server now tracks whether it created the
  connection itself and only closes owned connections.

## Breaking Changes

* Migrate exported Bitcoin types and dependencies to the btcd v2 modules.
  Source builds require Go 1.26.8, and L402 token payments require lnd v0.19.0-beta.

## Maintenance

* Retain the lnd v0.21.4 recovery fixes while migrating to btcd v2.
