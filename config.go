package lightmeter

import (
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	flags "github.com/jessevdk/go-flags"
)

const (
	// DefaultListenAddr is the address the daemon serves the pricesrpc
	// gRPC interface on when no other address is configured. Aperture's
	// dynamicprice.grpcaddress points here.
	DefaultListenAddr = "127.0.0.1:10020"

	// DefaultBundleTokens is the number of LLM tokens sold per bundle
	// when neither the daemon default nor a per-model override is set.
	DefaultBundleTokens = 1_000_000

	// DefaultEstimatedTokens is the per-request token estimate reserved
	// on authorization when the request body carries no max_tokens hint.
	DefaultEstimatedTokens = 4_096

	// DefaultMaxUnauthorizedBundles caps the number of never-authorized
	// (un-paid) bundles retained at once.
	DefaultMaxUnauthorizedBundles = 10_000

	// DefaultBasetenBaseURL is the base URL of Baseten's OpenAI
	// compatible Model APIs.
	DefaultBasetenBaseURL = "https://inference.baseten.co"

	// DefaultRefreshInterval is how often the price table re-reads the
	// upstream model list and the BTC-USD rate.
	DefaultRefreshInterval = 15 * time.Minute

	// DefaultWebhookPath is the path the billing webhook receiver is
	// mounted on.
	DefaultWebhookPath = "/webhooks/baseten"

	// DefaultRetentionDays is how many days of usage and provider
	// billing events are retained by default.
	DefaultRetentionDays = 90

	// maxMarginPercent is the sanity ceiling on the configured margin.
	maxMarginPercent = 10_000.0

	// maxUSDPerMTok is the sanity ceiling on a configured USD per
	// million token rate. It keeps a fat-fingered price from pushing
	// the msat conversion toward the int64 range.
	maxUSDPerMTok = 1_000_000.0
)

// ModelPricing holds the seller-side economics of a single model: what the
// upstream provider charges in USD, and optionally a per-model bundle size.
type ModelPricing struct {
	// InputUSDPerMTok is the upstream USD price per million input
	// (prompt) tokens.
	InputUSDPerMTok float64 `yaml:"inputusdpermtok"`

	// OutputUSDPerMTok is the upstream USD price per million output
	// (completion) tokens.
	OutputUSDPerMTok float64 `yaml:"outputusdpermtok"`

	// BundleTokens overrides the daemon-wide bundle size for this model
	// when positive.
	BundleTokens int64 `yaml:"bundletokens"`
}

// BTCUSDConfig configures how the daemon learns the BTC-USD rate used to
// convert upstream USD prices into millisatoshis.
type BTCUSDConfig struct {
	// StaticRate pins the BTC-USD rate to a fixed value. When positive,
	// no spot source is consulted; this is the regtest/dev mode.
	StaticRate float64 `yaml:"staticrate"`
}

// PricingConfig holds the knobs that turn upstream USD prices into the msat
// rates quoted to clients.
type PricingConfig struct {
	// MarginPercent is the markup applied on top of the upstream USD
	// price, in percent. A value of 25 sells tokens at 1.25 times the
	// upstream price.
	MarginPercent float64 `yaml:"marginpercent"`

	// DefaultUSD prices models that are discovered from the upstream
	// model list but have no explicit entry in the models map. When nil,
	// discovered models without an entry are not sellable.
	DefaultUSD *ModelPricing `yaml:"defaultusd"`

	// BTCUSD configures the USD to BTC conversion.
	BTCUSD BTCUSDConfig `yaml:"btcusd"`
}

// BasetenConfig configures the upstream Baseten Model APIs connection used
// for model discovery.
type BasetenConfig struct {
	// APIKey is the Baseten API key. Prefer APIKeyPath so the key stays
	// out of the config file.
	APIKey string `yaml:"apikey"`

	// APIKeyPath is a path to a file holding the Baseten API key.
	APIKeyPath string `yaml:"apikeypath"`

	// BaseURL is the base URL of the Model APIs.
	BaseURL string `yaml:"baseurl"`
}

// WebhookConfig configures the HTTP listener that ingests upstream billing
// webhooks for reconciliation.
type WebhookConfig struct {
	// ListenAddr is the address the webhook HTTP server listens on.
	// The webhook server is disabled when empty.
	ListenAddr string `yaml:"listenaddr"`

	// Secret is the shared HMAC secret webhook payloads are signed
	// with.
	Secret string `yaml:"secret"`

	// Path is the URL path the receiver is mounted on.
	Path string `yaml:"path"`

	// ReconToken is the bearer token required to read the
	// reconciliation report. The report endpoint is disabled when
	// empty.
	ReconToken string `yaml:"recontoken"`
}

// Config holds the daemon configuration. Values are read from an optional
// YAML config file and can be overridden through command line flags. The
// nested maps and structs can only be configured through the YAML file.
type Config struct {
	// ConfigFile points to an optional YAML config file.
	ConfigFile string `yaml:"-" long:"config" description:"Path to a YAML config file"`

	// ListenAddr is the address the pricesrpc gRPC server listens on.
	ListenAddr string `yaml:"listenaddr" long:"listenaddr" description:"Address to serve the prices gRPC interface on"`

	// TLSCertPath is the path to the TLS certificate to serve with. The
	// server speaks plaintext when unset.
	TLSCertPath string `yaml:"tlscertpath" long:"tlscertpath" description:"Path to the TLS certificate to serve with, plaintext when unset"`

	// TLSKeyPath is the path to the TLS key belonging to the
	// certificate.
	TLSKeyPath string `yaml:"tlskeypath" long:"tlskeypath" description:"Path to the TLS key to serve with"`

	// DBPath is the path to the sqlite database bundle balances and the
	// usage ledger are stored in.
	DBPath string `yaml:"dbpath" long:"dbpath" description:"Path to the sqlite database"`

	// DefaultModel is the model used to price requests whose model is
	// missing or unknown.
	DefaultModel string `yaml:"defaultmodel" long:"defaultmodel" description:"Model to price requests with an unknown or missing model as"`

	// BundleTokens is the daemon-wide number of LLM tokens sold per
	// bundle. Individual models can override it.
	BundleTokens int64 `yaml:"bundletokens" long:"bundletokens" description:"Default number of LLM tokens sold per bundle"`

	// EstimatedTokens is the per-request token estimate reserved on
	// authorization when the request carries no max_tokens hint.
	EstimatedTokens int64 `yaml:"estimatedtokens" long:"estimatedtokens" description:"Per-request token estimate reserved on authorization"`

	// MaxUnauthorizedBundles caps the number of never-authorized
	// (un-paid) bundles retained at once.
	MaxUnauthorizedBundles int `yaml:"maxunauthorizedbundles" long:"maxunauthorizedbundles" description:"Maximum number of never-authorized bundles retained"`

	// UnauthorizedBundleTTL is how long a never-authorized bundle is
	// retained before it is reaped.
	UnauthorizedBundleTTL time.Duration `yaml:"unauthorizedbundlettl" long:"unauthorizedbundlettl" description:"How long a never-authorized bundle is retained"`

	// RetentionDays bounds how long usage and provider billing events
	// are kept before the periodic sweep prunes them. Zero keeps them
	// forever.
	RetentionDays int `yaml:"retentiondays" long:"retentiondays" description:"Days to retain usage and provider billing events, 0 keeps them forever"`

	// RefreshInterval is how often the price table re-reads the upstream
	// model list and the BTC-USD rate.
	RefreshInterval time.Duration `yaml:"refreshinterval" long:"refreshinterval" description:"How often the price table refreshes"`

	// Models maps a model identifier to its seller-side pricing.
	Models map[string]*ModelPricing `yaml:"models"`

	// Pricing holds the margin and currency conversion knobs.
	Pricing PricingConfig `yaml:"pricing"`

	// Baseten configures the upstream model discovery.
	Baseten BasetenConfig `yaml:"baseten"`

	// Webhook configures the billing webhook receiver.
	Webhook WebhookConfig `yaml:"webhook"`
}

// NewConfig returns a Config populated with all default values.
func NewConfig() *Config {
	return &Config{
		ListenAddr:             DefaultListenAddr,
		BundleTokens:           DefaultBundleTokens,
		EstimatedTokens:        DefaultEstimatedTokens,
		MaxUnauthorizedBundles: DefaultMaxUnauthorizedBundles,
		RefreshInterval:        DefaultRefreshInterval,
		RetentionDays:          DefaultRetentionDays,
		Baseten: BasetenConfig{
			BaseURL: DefaultBasetenBaseURL,
		},
		Webhook: WebhookConfig{
			Path: DefaultWebhookPath,
		},
	}
}

// LoadConfig parses the given command line arguments, merges in the YAML
// config file when one is provided and validates the result. Command line
// flags take precedence over values from the config file.
func LoadConfig(args []string) (*Config, error) {
	// Pre-parse the command line to learn about a custom config file
	// location.
	cfg := NewConfig()
	parser := flags.NewParser(cfg, flags.Default)
	if _, err := parser.ParseArgs(args); err != nil {
		return nil, err
	}

	if cfg.ConfigFile != "" {
		b, err := os.ReadFile(cfg.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read config file "+
				"%s: %w", cfg.ConfigFile, err)
		}

		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("unable to parse config file "+
				"%s: %w", cfg.ConfigFile, err)
		}

		// Parse the command line again so that flags take precedence
		// over the config file.
		parser = flags.NewParser(cfg, flags.Default)
		if _, err := parser.ParseArgs(args); err != nil {
			return nil, err
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// basetenAPIKey resolves the configured Baseten API key, preferring the key
// file over the inline value. An empty return value disables upstream model
// discovery.
func (c *Config) basetenAPIKey() (string, error) {
	if c.Baseten.APIKeyPath != "" {
		b, err := os.ReadFile(c.Baseten.APIKeyPath)
		if err != nil {
			return "", fmt.Errorf("unable to read Baseten API "+
				"key file %s: %w", c.Baseten.APIKeyPath, err)
		}

		return strings.TrimSpace(string(b)), nil
	}

	return c.Baseten.APIKey, nil
}

// validate checks the configuration for consistency.
func (c *Config) validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listenaddr must be set")
	}

	if c.DBPath == "" {
		return fmt.Errorf("dbpath must be set")
	}

	if c.BundleTokens <= 0 {
		return fmt.Errorf("bundletokens must be positive, got %d",
			c.BundleTokens)
	}

	if c.EstimatedTokens < 0 {
		return fmt.Errorf("estimatedtokens must not be negative, "+
			"got %d", c.EstimatedTokens)
	}

	if (c.TLSCertPath == "") != (c.TLSKeyPath == "") {
		return fmt.Errorf("tlscertpath and tlskeypath must be set " +
			"together")
	}

	// The gRPC interface carries unauthenticated money-affecting calls
	// (ReportUsage debits balances), so serving it in plaintext is only
	// acceptable on loopback where aperture is co-located.
	if c.TLSCertPath == "" && !isLoopbackAddr(c.ListenAddr) {
		return fmt.Errorf("listenaddr %s is not loopback: plaintext "+
			"gRPC exposes unauthenticated metering calls, set "+
			"tlscertpath/tlskeypath or bind to 127.0.0.1",
			c.ListenAddr)
	}

	finite := func(name string, v float64) error {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a finite number, got "+
				"%v", name, v)
		}

		return nil
	}

	if err := finite("marginpercent", c.Pricing.MarginPercent); err != nil {
		return err
	}
	if c.Pricing.MarginPercent < 0 ||
		c.Pricing.MarginPercent > maxMarginPercent {

		return fmt.Errorf("marginpercent must be between 0 and %v, "+
			"got %v", maxMarginPercent, c.Pricing.MarginPercent)
	}

	err := finite("btcusd staticrate", c.Pricing.BTCUSD.StaticRate)
	if err != nil {
		return err
	}
	if c.Pricing.BTCUSD.StaticRate < 0 {
		return fmt.Errorf("btcusd staticrate must not be negative, "+
			"got %v", c.Pricing.BTCUSD.StaticRate)
	}

	if c.RefreshInterval > 0 && c.RefreshInterval < time.Minute {
		return fmt.Errorf("refreshinterval must be at least 1m, "+
			"got %v", c.RefreshInterval)
	}

	if c.RetentionDays < 0 {
		return fmt.Errorf("retentiondays must not be negative, got "+
			"%d", c.RetentionDays)
	}

	checkRates := func(name string, m *ModelPricing) error {
		if m == nil {
			return fmt.Errorf("model %s has no pricing "+
				"configured", name)
		}
		err := finite(name+" inputusdpermtok", m.InputUSDPerMTok)
		if err != nil {
			return err
		}
		err = finite(name+" outputusdpermtok", m.OutputUSDPerMTok)
		if err != nil {
			return err
		}
		if m.InputUSDPerMTok < 0 || m.OutputUSDPerMTok < 0 {
			return fmt.Errorf("model %s has negative USD rates",
				name)
		}
		if m.InputUSDPerMTok > maxUSDPerMTok ||
			m.OutputUSDPerMTok > maxUSDPerMTok {

			return fmt.Errorf("model %s has a USD rate above "+
				"the %v per MTok sanity ceiling", name,
				maxUSDPerMTok)
		}
		if m.BundleTokens < 0 {
			return fmt.Errorf("model %s has a negative bundle "+
				"size", name)
		}

		return nil
	}

	for name, m := range c.Models {
		if err := checkRates(name, m); err != nil {
			return err
		}
	}

	if c.Pricing.DefaultUSD != nil {
		err := checkRates("defaultusd", c.Pricing.DefaultUSD)
		if err != nil {
			return err
		}
	}

	// The default model must be priceable: either explicitly in the
	// models map, or via the default USD rates once discovery finds it.
	if c.DefaultModel != "" && c.Pricing.DefaultUSD == nil {
		if _, ok := c.Models[c.DefaultModel]; !ok {
			return fmt.Errorf("defaultmodel %s is not present in "+
				"the models map and no pricing.defaultusd "+
				"is set", c.DefaultModel)
		}
	}

	if len(c.Models) == 0 && c.Pricing.DefaultUSD == nil {
		return fmt.Errorf("no models configured: set a models map " +
			"or pricing.defaultusd")
	}

	if c.Webhook.ListenAddr != "" {
		if c.Webhook.Secret == "" {
			return fmt.Errorf("webhook.secret must be set when " +
				"the webhook listener is enabled")
		}
		if !strings.HasPrefix(c.Webhook.Path, "/") {
			return fmt.Errorf("webhook.path %q must start "+
				"with /", c.Webhook.Path)
		}
	}

	return nil
}

// isLoopbackAddr reports whether the listen address binds a loopback
// interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
