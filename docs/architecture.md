# Architecture: L402 inference payments, end to end

This document walks the full path of a paid inference request, from a coding
agent deciding to call a model to the satoshis landing and the tokens being
drawn down. Three deployables cooperate, each owning one concern:

| Component | Repo | Role |
|---|---|---|
| pi-402 | [lightninglabs/pi-402](https://github.com/lightninglabs/pi-402) | The buyer. A [Pi](https://pi.dev) extension with an embedded walletdk wallet that pays L402 challenges. |
| aperture | [lightninglabs/aperture](https://github.com/lightninglabs/aperture) | The toll booth. A generic L402 reverse proxy that mints and verifies credentials. |
| lightmeter | this repo | The seller's brain. Prices tokens, tracks bundles, reconciles the books. |

Behind all three sits the actual inference provider (Baseten's
OpenAI-compatible [Model APIs](https://docs.baseten.co/inference/model-apis/overview)),
which knows nothing about Lightning. The proxy holds the provider API key;
the buyer never sees it.

```
pi-402 (buyer)                aperture (proxy)              lightmeter (seller)
     │                             │                              │
     │  POST /v1/chat/completions  │                              │
     ├────────────────────────────►│  GetPrice / AuthorizeRequest │
     │                             ├─────────────────────────────►│
     │   402 + invoice + macaroon  │      ChallengeMinted         │
     │◄────────────────────────────┤─────────────────────────────►│
     │  (pays over Lightning)      │                              │
     │  retry with L402 header     │                              │
     ├────────────────────────────►│──── proxied to Baseten ────► │
     │   streamed response         │      ReportUsage             │
     │◄────────────────────────────┤─────────────────────────────►│
```

## Why three pieces instead of one

The split follows what each party can be generic about.

Aperture stays a *generic* proxy. It understands HTTP, L402 macaroons, and
Lightning invoices, and it asks a price server what things cost. It has no
idea what a token is. That keeps the metering extension upstreamable: any
metered service (inference, storage, video) can sit behind the same proxy.

Lightmeter is *product-specific*. It knows that the good being sold is LLM
tokens, that Baseten charges some USD per million of them, what margin the
seller wants, and how large a prepaid bundle should be. None of that belongs
in a proxy. The two talk over one narrow gRPC interface,
`pricesrpc.Prices`, so either side can be replaced.

Pi-402 is the mirror image on the buyer side: Pi core is never modified;
the extension wraps the model provider's HTTP client in a paying `fetch`
that answers 402 challenges from a walletdk wallet, within budget-policy
ceilings.

## The four RPC touchpoints

Aperture consults lightmeter at four moments (the `metered: true` mode of
aperture's `dynamicprice` config, added in
[aperture#247](https://github.com/lightninglabs/aperture/pull/247)):

1. **GetPrice.** A request arrives with no credential. Aperture asks what a
   bundle costs for the model named in the request body. Lightmeter quotes
   the bundle price in satoshis at the model's blended rate.
2. **ChallengeMinted.** Aperture minted a 402 challenge (macaroon +
   invoice) for that quote. Lightmeter books a bundle against the
   credential's token ID, remembering the model and price. The booking is
   idempotent and unpaid bookings are capped and reaped, so challenge spam
   cannot grow state without bound.
3. **AuthorizeRequest.** A request arrives bearing a valid L402. Lightmeter
   checks the bundle: enough balance, and the request's model matches the
   booked model (a cheap-model bundle cannot be spent on an expensive
   model). Denials carry a quote for the next bundle so aperture can mint a
   fresh challenge without another GetPrice round trip.
4. **ReportUsage.** The response finished streaming. Aperture captured the
   tail of the body; lightmeter extracts the OpenAI `usage` object from it
   and debits actual consumption from the bundle.

Between authorize and report, the bundle carries a *reservation* (the
request's `max_tokens`, or a configured estimate) so that N concurrent
requests against a nearly empty bundle cannot all slip through.

## The bundle lifecycle

One Lightning payment buys one bundle of N tokens (one million by default).
The client then draws it down across as many requests as the balance
allows. The states:

- **Booked**: challenge minted, invoice unpaid. Reaped after a TTL, capped
  in count.
- **Active**: at least one authorized request has used it (which implies
  the invoice was paid, since aperture only authorizes valid preimages).
  Balance decreases with each usage report, never below zero.
- **Exhausted**: balance hit zero. The next AuthorizeRequest is denied with
  a fresh quote; the buyer pays again and continues with a new credential.

The buyer's token cache (in `@aperture/client`) holds the credential until
it stops working, so the per-request happy path costs zero extra round
trips: present the L402, get the stream.

## Where the mechanics live

The booking, reservation, and draw-down logic is not implemented in this
repo. It lives in aperture's `meterd` package, and lightmeter embeds it as
a library through two seams:

- `meterd.Store`: where bundle balances live. meterd ships a JSON file
  store; lightmeter supplies sqlite (see
  [operations](operations.md#the-database)).
- `meterd.RateSource`: what models cost and how big their bundles are.
  meterd ships a static config map; lightmeter supplies a refreshing table
  fed by Baseten's model list and the BTC-USD rate (see
  [pricing](pricing.md)).

The reference `meterd` binary and lightmeter therefore share one metering
brain, and a fix in either's mechanics lands in both.

## What lightmeter adds

Everything a real seller needs beyond the mechanics:

- **Real prices**: USD per million tokens per model, margin on top,
  converted at the live BTC-USD rate ([pricing.md](pricing.md)).
- **Catalog discovery**: new models on Baseten's `/v1/models` become
  sellable without a config change.
- **A durable ledger**: every debit is a row, so the seller can answer
  "how many tokens did we sell last week, per model".
- **Reconciliation**: Baseten's billing webhooks are ingested and compared
  against the ledger; a growing negative delta means the provider is
  billing usage that was never metered
  ([operations.md](operations.md#reconciliation)).
