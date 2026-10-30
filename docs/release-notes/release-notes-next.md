# Next release

## Breaking Changes

* Migrate exported Bitcoin types and dependencies to the btcd v2 modules.
  Source builds require Go 1.26.8, and L402 token payments require lnd v0.19.0-beta.

## Bug Fixes

* Disable MPTCP listeners by default, restoring plain TCP for `poold` and
  mitigating the demonstrated CVE-2026-98432 exploit path on vulnerable Linux
  kernels. Existing binaries can apply the same mitigation with
  `GODEBUG=multipathtcp=0` and a restart. A patched kernel is still required to
  fix the underlying vulnerability.

## Maintenance

* Retain the lnd v0.21.4 recovery fixes while migrating to btcd v2.
