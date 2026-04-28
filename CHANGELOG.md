# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `messaging.SubscribeAll(c, fn)` —— wildcard 订阅 API。一个 inbox + 一个
  worker 接收 channel 上所有 topic 的 envelope，**跨 topic 保留 wire 顺序**。
  
  动机：现有的 `Subscribe[T proto.Message]` 把 typed dispatch 拉进 vertex
  框架（topic 由 `T.Descriptor().FullName()` 派生），结果是每个 type T 一个
  独立 inbox + 独立 worker。如果消费方对多个 type T1/T2 都 Subscribe，
  跨 type 顺序由 worker 抢调度决定 —— **没有顺序保证**。
  
  对消费方需要"per-channel 跨 type 严格顺序"的场景（典型：彩种 issue
  生命周期 opening → stopping → drawing → finished → next opening 必须
  按业务顺序送给客户端，[L8CHAT/l8-game-server #126](#) issue），typed
  Subscribe 不可用。
  
  SubscribeAll 把所有 envelope 通过单 inbox + 单 worker 漏出 —— 因为
  receiveLoop 是单 goroutine，enqueue 顺序 = wire 顺序，handler 看到的
  顺序 = transport deliver 顺序。
  
  权衡 vs Subscribe[T]：消费方自己按 `Envelope.Topic` 做 demux + proto
  unmarshal。vertex 不再插手 typed dispatch —— 这本来就该是消费层的事。
  
- `messaging.WildcardTopic` 常量（`"*"`）—— wildcard subscriber 的 drop
  在 `ChannelStats.EventsDropped` 里聚合到这个 key 下。
  
- 测试覆盖：
  - `TestSubscribeAll_ReceivesAllTopics`：wildcard 拿全集多 topic
  - `TestSubscribeAll_PreservesCrossTopicOrder`：100 条交替 type 验证 order
    严格保持
  - `TestSubscribeAll_CoexistsWithTypedSubscribe`：wildcard 和 typed 同时
    订阅互不影响

### Changed

- `Channel.dispatchEvent` 在原有 topic-typed fan-out 后，还会向所有
  wildcard subscribers fan-out（drop-on-full 同款 backpressure）。
  原 typed 订阅行为不变。

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
