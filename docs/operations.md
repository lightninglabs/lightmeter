# Operations: running lightmeter behind an aperture

This document covers deployment: wiring the two daemons together, the
database, reconciliation against Baseten's billing, and the failure modes
worth alarming on.

## Minimal deployment

Two processes and one config file each.

**lightmeter** (`lightmeter.yaml`, full reference in
[`lightmeter.sample.yaml`](../lightmeter.sample.yaml)):

```yaml
listenaddr: "127.0.0.1:10020"
dbpath: "/var/lib/lightmeter/lightmeter.db"
defaultmodel: "zai-org/GLM-4.7"
bundletokens: 1000000

models:
  "zai-org/GLM-4.7":
    inputusdpermtok: 0.55
    outputusdpermtok: 2.15

pricing:
  marginpercent: 25
```

**aperture**, pointing its inference service at lightmeter and at Baseten:

```yaml
services:
  - name: "inference"
    hostregexp: '^.*$'
    pathregexp: '^/v1/.*$'
    address: "inference.baseten.co:443"
    protocol: https
    headers:
      Authorization: "Bearer <baseten-api-key>"
    dynamicprice:
      enabled: true
      grpcaddress: "127.0.0.1:10020"
      insecure: true
      metered: true
```

`metered: true` is the switch that makes aperture consult the price server
on every authenticated request and stream usage reports back; without it,
lightmeter only ever sees GetPrice. The Baseten API key lives in aperture's
service headers; lightmeter needs a key only when model discovery is
enabled (`baseten.apikeypath`).

Keep both listeners on localhost or a private network. The gRPC interface
trusts its caller (aperture) completely, and the webhook listener, when
enabled, is the only piece that should ever face the internet.

For the buyer side of a full regtest rehearsal (funded wallet, Pi with the
pi-402 extension, this stack fronting a fake model), see pi-402's
[`dev/regtest/`](https://github.com/lightninglabs/pi-402/tree/main/dev/regtest)
harness, which was used to validate this exact topology end to end.

## The database

One sqlite file (WAL mode, single writer) with three tables:

- `bundles`: the live balances, keyed by L402 token ID. What
  AuthorizeRequest reads and ReportUsage debits.
- `usage_events`: append-only ledger, one row per debit (token, model,
  tokens, timestamp). This is the seller's sales record.
- `provider_events`: billing events ingested from webhooks, raw payload
  included.

Backing up the file (or WAL-checkpointing then copying) preserves all
seller state. Deleting it forgets outstanding bundles, which means paid
clients lose their balances: treat it like the money database it is.

In-flight reservations are deliberately *not* persisted. They bound
concurrent overdraw within a running process; on restart there are no
in-flight requests, so an empty reservation table is the correct state.

## Reconciliation

Metering happens at the proxy edge, billing happens at Baseten, and the two
can drift: a crashed stream that never produced a usage object, a
compressed tail that could not be parsed, a bug. Reconciliation makes the
drift visible instead of silent.

Enable the webhook listener:

```yaml
webhook:
  listenaddr: "127.0.0.1:10021"
  secret: "<shared-hmac-secret>"
  path: "/webhooks/baseten"
```

Point Baseten's billing webhooks at it (through your TLS-terminating
ingress). Payloads are verified with HMAC-SHA256 over the raw body
(`X-Baseten-Signature`, `v1=<hex>` or bare hex), stored verbatim, and
parsed tolerantly for model and token counts. Events carrying an ID are
deduped on redelivery.

Then compare the books:

```
$ curl -s 'localhost:10021/recon?since=2026-07-01T00:00:00Z' | jq
{
  "since": "2026-07-01T00:00:00Z",
  "models": {
    "zai-org/GLM-4.7": {
      "metered_tokens": 1482930,
      "provider_tokens": 1490112,
      "delta_tokens": -7182
    }
  }
}
```

`delta_tokens` is metered minus provider-billed. A small steady negative
delta is expected (responses that failed mid-stream still cost Baseten
tokens but produced no usage object). A *growing* negative delta is the
alarm condition: the provider is billing usage the meter never charged
for, i.e. the seller is leaking money. Alert on its slope, not its value.

## Failure modes

| Failure | Behavior | Operator action |
|---|---|---|
| Coinbase spot unreachable at startup | Daemon refuses to start (no rate table, nothing can be priced). | Fix connectivity, or pin `pricing.btcusd.staticrate`. |
| Coinbase spot fails after startup | Previous snapshot keeps serving; refresh retries on the interval. | Alarm on repeated refresh failures: stale prices during a BTC move are bounded but real exposure. |
| Baseten `/v1/models` fails | Discovery skipped; configured models keep pricing. | None required. |
| lightmeter down entirely | Aperture's metered path fails closed: no authorization means no request reaches Baseten. Denied requests, not free ones. | Restart; balances are in sqlite, buyers resume on their existing credentials. |
| Webhook secret mismatch | Events rejected with 401, logged. | Rotate the shared secret on both ends. |
| Response tail unparseable (no usage object in a complete 200) | Nothing debited; loud warning logged (this is the money-leak case the recon report catches). | Investigate the model's response shape; check for a non-identity Content-Encoding upstream. |

The general stance: pricing degrades gracefully (stale beats dead), but
authorization fails closed (unpriced beats free).
