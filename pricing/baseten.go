package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ModelLister lists the model identifiers currently offered by the upstream
// inference provider.
type ModelLister interface {
	// ListModels returns the upstream model identifiers.
	ListModels(ctx context.Context) ([]string, error)
}

// BasetenClient talks to Baseten's OpenAI compatible Model APIs. Only the
// model listing endpoint is used: the daemon discovers which slugs are
// currently sellable and joins them against the configured USD price table.
type BasetenClient struct {
	// BaseURL is the base URL of the Model APIs, e.g.
	// https://inference.baseten.co.
	BaseURL string

	// APIKey is the Baseten API key sent as a bearer token.
	APIKey string

	// Client is the HTTP client requests are made with. A default
	// client with a short timeout is used when nil.
	Client *http.Client
}

// ListModels fetches the /v1/models listing and returns the model
// identifiers.
func (c *BasetenClient) ListModels(ctx context.Context) ([]string, error) {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	url := c.BaseURL + "/v1/models"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, url, nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unable to list upstream models: %w",
			err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list request failed with "+
			"status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("unable to decode model list: %w", err)
	}

	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}

	return models, nil
}
