# Pricing: from Baseten's USD sticker to msat per token

Everything the daemon quotes derives from one pipeline: an upstream USD
price per million tokens, a margin, and a BTC-USD rate. This document
follows a price through that pipeline and explains the policy knobs around
it.

## The conversion

Baseten publishes model prices in USD per million tokens, split by
direction (input/prompt vs output/completion). Lightning amounts are
millisatoshis. The bridge, `pricing.MsatPerToken`:

```
msat_per_token = usd_per_mtok / 1e6        USD per token
               / btc_usd                    BTC per token
               * 1e8                        sat per token
               * 1e3                        msat per token
               * (1 + margin/100)

             = usd_per_mtok * 1e5 * (1 + margin/100) / btc_usd
```

A worked example: GLM-4.7 output at $2.15 per MTok, 25% margin, BTC at
$100,000:

```
2.15 * 100000 * 1.25 / 100000 = 2.6875  →  3 msat per token
```

Two rounding rules protect the seller:

- The result **rounds up** (`math.Ceil`), never down.
- Any positive USD price floors at **1 msat per token**. A cost never
  converts to free, no matter how strong bitcoin gets.

These per-token msat rates are exactly what the embedded meterd core prices
with: a bundle quote is `ceil(bundleTokens * (input + output) / 2 / 1000)`
sats (the blended rate, assuming an even prompt/completion mix), and usage
reports debit at the exact per-direction rates when the response's usage
object splits them.

## Where each input comes from

**USD prices.** The `models` map in the config is authoritative: each entry
carries `inputusdpermtok` and `outputusdpermtok` (copy them from Baseten's
price list). Entries survive refreshes and are never overridden by
discovery.

**Discovered models.** With a Baseten API key configured, the table
periodically fetches `/v1/models` and adds any model missing from the
config at the `pricing.defaultusd` rates. Set conservative defaults: they
are the price of being surprised by a new model. Omit `defaultusd` to make
discovery additive-only in catalog terms but unsellable in practice, i.e.
only explicitly priced models resolve.

**BTC-USD.** Two sources:

- `pricing.btcusd.staticrate` pins the rate. Use for regtest and tests.
- Otherwise the daemon polls Coinbase's public spot endpoint
  (`/v2/prices/BTC-USD/spot`) on the refresh interval.

**Margin.** `pricing.marginpercent` applies to every model uniformly, on
top of the upstream USD price. 25 means clients pay 1.25 times what Baseten
charges the seller.

One caution on sizing it: bundles are *priced* at the blended
(input+output)/2 rate, but usage *debits* raw token counts regardless of
direction, and the buyer chooses the mix. A worst-case all-output buyer
costs the seller the output rate while having paid the blend. The margin
that makes even that buyer non-loss-making is `(out - in) / (out + in)`:
about 59% for a $0.55/$2.15 model. Set the margin above that bound, or
accept that output-heavy workloads may run below cost until direction
weighted debiting lands upstream.

## Refresh semantics

The table recomputes as one atomic snapshot every `refreshinterval`
(15 minutes by default): read the BTC-USD rate, list the models, convert
everything, swap the map under a lock. Requests always price against a
complete, consistent snapshot.

Failure is asymmetric by design:

- The **initial** snapshot must succeed or the daemon refuses to start. A
  price server with no prices would fail every request anyway; better to
  fail loudly at startup.
- A **later** failed refresh logs and keeps the previous snapshot. A flaky
  Coinbase or Baseten degrades to slightly stale prices, not an outage.
  Watch the logs for repeated refresh failures: stale prices during a
  sharp BTC move are a real (bounded) exposure.

Model *removal* is deliberately conservative: a model that disappears from
`/v1/models` but remains in the config keeps pricing, since config entries
are authoritative. Outstanding bundles for a model that stops resolving
deny with the default model's quote.

## Bundle policy

One L402 payment buys one bundle. Size is policy:

- `bundletokens` sets the daemon-wide bundle size (default one million
  tokens).
- A per-model `bundletokens` override tunes the purchase size to the
  model's price, so a premium model can sell in smaller increments. A
  bundle should cost enough sats that one Lightning payment amortizes
  well, but little enough that a client's unspent balance stays an
  acceptable float.

The trade is prepayment float against payment frequency. At 1M tokens of
GLM-4.7 with the numbers above, a bundle quotes roughly 2,000 sats; a
buyer burning 1,000 tokens per request pays about once per thousand
requests.

## What the client sees

None of this machinery is visible to the buyer. The 402 challenge carries a
plain satoshi invoice; the buyer's budget policy (pi-402 asserts per-request
and daily ceilings on the quoted total outflow) decides whether to pay it.
Per-token prices, margin, and the BTC-USD rate stay server-side, which is
what lets the seller adjust them without a protocol change.
