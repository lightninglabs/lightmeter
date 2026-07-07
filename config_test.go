package lightmeter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// validConfig returns a config that passes validation, for the negative
// cases to perturb.
func validConfig() *Config {
	cfg := NewConfig()
	cfg.DBPath = "/tmp/test.db"
	cfg.Models = map[string]*ModelPricing{
		"glm": {InputUSDPerMTok: 0.55, OutputUSDPerMTok: 2.15},
	}
	cfg.DefaultModel = "glm"

	return cfg
}

// TestValidateBaseline checks that the reference config is accepted.
func TestValidateBaseline(t *testing.T) {
	require.NoError(t, validConfig().validate())
}

// TestValidatePlaintextNonLoopback checks that a plaintext gRPC listener
// is refused off loopback: the metering calls are unauthenticated and
// move money.
func TestValidatePlaintextNonLoopback(t *testing.T) {
	cfg := validConfig()
	cfg.ListenAddr = "0.0.0.0:10020"
	require.Error(t, cfg.validate())

	// TLS makes the same bind acceptable.
	cfg.TLSCertPath = "/tls.cert"
	cfg.TLSKeyPath = "/tls.key"
	require.NoError(t, cfg.validate())

	// Loopback binds stay fine in plaintext.
	cfg = validConfig()
	cfg.ListenAddr = "localhost:10020"
	require.NoError(t, cfg.validate())
}

// TestValidateWebhookPath checks that an enabled webhook listener needs a
// well-formed mount path and a secret.
func TestValidateWebhookPath(t *testing.T) {
	cfg := validConfig()
	cfg.Webhook.ListenAddr = "127.0.0.1:10021"
	cfg.Webhook.Secret = "s"
	cfg.Webhook.Path = "no-slash"
	require.Error(t, cfg.validate())

	cfg.Webhook.Path = "/hooks"
	require.NoError(t, cfg.validate())

	cfg.Webhook.Secret = ""
	require.Error(t, cfg.validate())
}

// TestValidateRateSanity checks the pricing input bounds.
func TestValidateRateSanity(t *testing.T) {
	cfg := validConfig()
	cfg.Pricing.MarginPercent = -1
	require.Error(t, cfg.validate())

	cfg = validConfig()
	cfg.Models["glm"].InputUSDPerMTok = maxUSDPerMTok * 2
	require.Error(t, cfg.validate())

	cfg = validConfig()
	cfg.RefreshInterval = 1
	require.Error(t, cfg.validate())
}

// TestValidateDiscoveryDefaultModel checks that the default model may be
// priced through discovery defaults rather than the models map.
func TestValidateDiscoveryDefaultModel(t *testing.T) {
	cfg := NewConfig()
	cfg.DBPath = "/tmp/test.db"
	cfg.DefaultModel = "discovered/model"
	cfg.Pricing.DefaultUSD = &ModelPricing{
		InputUSDPerMTok:  0.5,
		OutputUSDPerMTok: 1.5,
	}
	require.NoError(t, cfg.validate())

	// Without discovery defaults, an unknown default model is still an
	// error.
	cfg.Pricing.DefaultUSD = nil
	require.Error(t, cfg.validate())
}
