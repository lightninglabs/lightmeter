# lightmeter

lightmeter is the production token-metering controller for L402 priced LLM
inference. It sits behind an [aperture](https://github.com/lightninglabs/aperture)
proxy as its dynamic price server and owns the seller economics: what a
bundle of tokens costs, how large a bundle is, and whether the books balance
against the upstream provider's billing.

In photographic terms: aperture controls what gets through, lightmeter
decides the exposure.

## Topology

```
client (L402 buyer) ──► aperture (proxy, mints/verifies L402)
                            │  pricesrpc (gRPC)
                            ▼
                        lightmeter (this daemon)
                            │  /v1/models, billing webhooks
                            ▼
                        Baseten Model APIs (the actual inference)
```

Aperture stays a generic proxy: it forwards inference requests, mints 402
challenges, and consults its configured price server on every request via
the `pricesrpc.Prices` interface (`GetPrice`, `ChallengeMinted`,
`AuthorizeRequest`, `ReportUsage`, see aperture's `dynamicprice.metered`
mode). lightmeter implements that interface. One L402 payment buys a bundle
of tokens; each metered response's usage object draws the bundle down; an
exhausted bundle denies with a quote for the next one, prompting aperture
to mint a fresh challenge.

The metering mechanics (bundle booking, reservation against concurrent
overdraw, draw-down) are embedded from aperture's `meterd` package via its
`Store` and `RateSource` seams. What this daemon adds on top is the
product-specific business logic that has no place in a generic proxy:

- **Real prices.** Upstream USD per-million-token prices per model, a
  configurable margin, and conversion to msat at the live BTC-USD rate
  (Coinbase spot, or a pinned rate for regtest).
- **Model discovery.** The sellable catalog auto-refreshes from the
  provider's `/v1/models`; newly listed models price at the configured
  default USD rates.
- **Bundle policy.** A daemon-wide bundle size with per-model overrides.
- **A real database.** Bundle balances and a durable usage ledger live in
  sqlite instead of meterd's JSON file.
- **Reconciliation.** An HMAC-verified webhook receiver ingests the
  provider's billing events, and `GET /recon` compares the tokens we
  debited from client bundles against the tokens the provider billed us
  for, per model.

## Running

```
go build ./cmd/lightmeterd
./lightmeterd --config lightmeter.yaml
```

See [`lightmeter.sample.yaml`](lightmeter.sample.yaml) for the full set of
knobs. The minimum viable config is a database path, one priced model, and
a BTC-USD source. Point aperture at it:

```yaml
# aperture.yaml
services:
  - name: "inference"
    ...
    dynamicprice:
      enabled: true
      grpcaddress: "127.0.0.1:10020"
      insecure: true
      metered: true
```

## Docs

- [Architecture](docs/architecture.md): the end-to-end system across
  pi-402, aperture and lightmeter, and the bundle lifecycle.
- [Pricing](docs/pricing.md): the USD to msat pipeline, refresh semantics
  and bundle policy.
- [Operations](docs/operations.md): deployment, the database,
  reconciliation and failure modes.

## Status

The buyer-side counterpart is the [pi-402](https://github.com/lightninglabs/pi-402)
Pi extension, which pays these challenges from an embedded walletdk wallet.
The webhook payload parsing is deliberately tolerant: billing event schemas
vary, so every event is stored verbatim for audit and the token counts are
extracted best-effort.
