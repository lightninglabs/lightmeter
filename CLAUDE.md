# lightmeter

## Purpose

The production token-metering controller for L402 priced LLM inference. It
implements the `pricesrpc.Prices` gRPC service that an
[aperture](https://github.com/lightninglabs/aperture) proxy consults in its
`dynamicprice.metered` mode, and owns the seller economics: upstream USD
prices with a margin, converted to msat at the live BTC-USD rate, per-model
bundle sizing, a sqlite ledger, and billing-webhook reconciliation.

Camera theme: aperture controls what gets through, lightmeter decides the
exposure.

## Module Structure

- `lightmeter.go` — `Daemon` wiring and `Main()`: builds the store and the
  pricing table, hands both to `meterd.NewServer` via `WithStore` /
  `WithRateSource`, starts the gRPC and webhook listeners.
- `config.go` — YAML + flags config (`LoadConfig`), following meterd's
  pattern. Nested knobs (models map, pricing, webhook) are YAML-only.
- `pricing/` — the economics. `Table` implements `meterd.RateSource`:
  per-model USD prices plus margin converted at the BTC-USD rate
  (`MsatPerToken`), refreshed periodically. `BasetenClient` discovers models
  from `/v1/models`; `BTCUSDSource` is Coinbase spot or a static pin.
- `store/` — `SQLiteStore` implements `meterd.Store` (bundles table),
  appends every debit to the `usage_events` ledger, stores webhook
  `provider_events`, and computes `ReconReport`.
- `webhook/` — HMAC-SHA256 verified billing-webhook receiver plus the
  `GET /recon` reconciliation endpoint.
- `cmd/lightmeterd/` — thin main.

## Relationships

- **Embeds**: `github.com/lightninglabs/aperture/meterd` (metering
  mechanics: booking, reservation, draw-down), pinned in go.mod to the
  `inference-pricing` branch of
  [aperture#247](https://github.com/lightninglabs/aperture/pull/247).
- **Serves**: an aperture proxy over `pricesrpc` (GetPrice,
  ChallengeMinted, AuthorizeRequest, ReportUsage).
- **Buys from**: [Baseten Model APIs](https://docs.baseten.co/inference/model-apis/overview)
  (model discovery, billing webhooks). The proxy holds the actual API key;
  this daemon only needs one for discovery.
- **Buyer counterpart**: [pi-402](https://github.com/lightninglabs/pi-402),
  the Pi extension that pays the L402 challenges this daemon prices.

## Invariants

- All metering mechanics live in meterd; this repo never reimplements
  booking/reservation/draw-down logic, it supplies `Store` and `RateSource`.
- Rounding never favors the client: msat rates round up (`MsatPerToken`
  floors at 1 msat for any positive USD price).
- go.mod must mirror aperture's `replace` directives (protobuf hex-display,
  ellemouton/lndclient); replaces do not apply transitively.
- A failed table refresh keeps the previous snapshot; only the initial
  refresh (at `NewTable`) is fatal.
- Configured models are authoritative; `/v1/models` discovery only adds
  models at `pricing.defaultusd` rates, never overrides config.
- Webhook payloads are stored verbatim (`provider_events.raw`); parsing is
  best-effort and never a precondition for storage.
- In-flight reservations are memory-only by design (they track live
  requests); balances and the usage ledger are durable in sqlite.

## Deep Docs

- [`docs/architecture.md`](docs/architecture.md) — the end-to-end system
  and the bundle lifecycle.
- [`docs/pricing.md`](docs/pricing.md) — the USD to msat pipeline and the
  bundle policy.
- [`docs/operations.md`](docs/operations.md) — config, aperture wiring,
  reconciliation, failure modes.
