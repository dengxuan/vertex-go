# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.0] - 2026-04-24

Adds one surface hook needed by downstream SDK wrappers and ships the
Channel.Close drain fix that was the counterpart to vertex-dotnet 1.0.1's
disconnect-grace-period patch — the two sides together close the reverse-
RPC race that showed up during the first production-style integration
(L8CHAT Gaming + l8-game-server).

### Added

- `messaging.WithConnectionChangeListener`: lets a downstream wrapper
  (like gaming-go-sdk / payment-go-sdk) observe `ConnectionEvent`s
  without taking ownership of the transport's `Connections()` channel,
  which is needed so the wrapper can translate events into its own
  `ConnectionState` model while the messaging channel still consumes
  them for its own bookkeeping.

### Fixed

- `Channel.Close` now drains in-flight request dispatches before tearing
  down the transport — the symmetric fix to vertex-dotnet's grace-period
  patch for the case where a Gaming server gracefully shuts down mid-
  reverse-RPC (e.g. OrderSubmit in flight when the platform restarts).
  Previously the Go side's close would cancel pending invokes before
  the last frames drained; together with vertex-dotnet 1.0.1 this path
  is now race-free.

### Tests & bench

- 30-second concurrent stress test (`messaging`): asserts no goroutine
  leaks and bounded heap under sustained Publish + Invoke load.
- `Publish` / `Invoke` BenchmarkDotNet-equivalent Go benchmarks for
  regression baselines.

## [1.1.0] - earlier

See git history.

## [1.0.0] - earlier

Initial stable release.
