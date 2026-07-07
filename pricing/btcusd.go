package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// BTCUSDSource supplies the BTC-USD rate used to convert upstream USD
// prices into millisatoshis.
type BTCUSDSource interface {
	// Rate returns the current BTC-USD rate.
	Rate(ctx context.Context) (float64, error)
}

// StaticBTCUSD is a fixed BTC-USD rate, used for regtest and development
// where a live spot feed is unwanted.
type StaticBTCUSD float64

// Rate returns the fixed rate.
func (s StaticBTCUSD) Rate(context.Context) (float64, error) {
	if s <= 0 {
		return 0, fmt.Errorf("static BTC-USD rate must be positive, "+
			"got %v", float64(s))
	}

	return float64(s), nil
}

// CoinbaseSpot reads the BTC-USD spot price from Coinbase's public price
// endpoint.
type CoinbaseSpot struct {
	// Client is the HTTP client requests are made with. A default
	// client with a short timeout is used when nil.
	Client *http.Client
}

// coinbaseSpotURL is the public, unauthenticated spot price endpoint.
const coinbaseSpotURL = "https://api.coinbase.com/v2/prices/BTC-USD/spot"

// Rate fetches the current BTC-USD spot price.
func (c *CoinbaseSpot) Rate(ctx context.Context) (float64, error) {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, coinbaseSpotURL, nil,
	)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("unable to fetch BTC-USD spot rate: %w",
			err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("BTC-USD spot request failed with "+
			"status %d", resp.StatusCode)
	}

	var payload struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("unable to decode BTC-USD spot "+
			"response: %w", err)
	}

	rate, err := strconv.ParseFloat(payload.Data.Amount, 64)
	if err != nil {
		return 0, fmt.Errorf("unable to parse BTC-USD spot amount "+
			"%q: %w", payload.Data.Amount, err)
	}

	if rate <= 0 {
		return 0, fmt.Errorf("non-positive BTC-USD spot rate %v",
			rate)
	}

	return rate, nil
}
