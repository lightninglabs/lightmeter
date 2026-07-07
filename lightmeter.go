// Package lightmeter is the production token-metering controller for L402
// priced inference. It embeds the metering mechanics of aperture's meterd
// package (bundle booking, reservation, draw-down) and supplies the seller
// economics around them: upstream USD prices with a margin converted at the
// live BTC-USD rate, per-model bundle sizing, a sqlite ledger, and billing
// webhook reconciliation.
//
// In photographic terms: aperture controls what gets through, lightmeter
// decides the exposure.
package lightmeter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lightninglabs/aperture/meterd"
	"github.com/lightninglabs/lightmeter/pricing"
	"github.com/lightninglabs/lightmeter/store"
	"github.com/lightninglabs/lightmeter/webhook"
)

// Daemon bundles the running services: the meterd pricesrpc server fronted
// by our pricing table and sqlite store, plus the optional webhook
// listener.
type Daemon struct {
	cfg *Config
	log *slog.Logger

	store   *store.SQLiteStore
	table   *pricing.Table
	meter   *meterd.Server
	webhook *webhook.Server
}

// NewDaemon wires a Daemon from the given configuration.
func NewDaemon(ctx context.Context, cfg *Config) (*Daemon, error) {
	logger := slog.Default()

	// The eviction age floor matches the janitor's TTL: within it a
	// paid-but-not-yet-used bundle is immune to count eviction, and
	// beyond it the janitor reaps unauthorized bundles anyway.
	minEvictionAge := cfg.UnauthorizedBundleTTL
	if minEvictionAge <= 0 {
		minEvictionAge = meterd.DefaultUnauthorizedBundleTTL
	}

	st, err := store.NewSQLiteStore(store.StoreConfig{
		Path:            cfg.DBPath,
		MaxUnauthorized: cfg.MaxUnauthorizedBundles,
		MinEvictionAge:  minEvictionAge,
		Retention: time.Duration(cfg.RetentionDays) *
			24 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to open store: %w", err)
	}

	// The BTC-USD source: a pinned rate for regtest and development,
	// the public Coinbase spot feed otherwise.
	var btcUSD pricing.BTCUSDSource = &pricing.CoinbaseSpot{}
	if cfg.Pricing.BTCUSD.StaticRate > 0 {
		btcUSD = pricing.StaticBTCUSD(cfg.Pricing.BTCUSD.StaticRate)
	}

	// Model discovery only runs with an API key configured; without
	// one, the configured models map is the whole catalog.
	var lister pricing.ModelLister
	apiKey, err := cfg.basetenAPIKey()
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		lister = &pricing.BasetenClient{
			BaseURL: cfg.Baseten.BaseURL,
			APIKey:  apiKey,
		}
	}

	models := make(map[string]*pricing.ModelUSD, len(cfg.Models))
	for name, m := range cfg.Models {
		models[name] = &pricing.ModelUSD{
			InputUSDPerMTok:  m.InputUSDPerMTok,
			OutputUSDPerMTok: m.OutputUSDPerMTok,
			BundleTokens:     m.BundleTokens,
		}
	}

	var defaultUSD *pricing.ModelUSD
	if d := cfg.Pricing.DefaultUSD; d != nil {
		defaultUSD = &pricing.ModelUSD{
			InputUSDPerMTok:  d.InputUSDPerMTok,
			OutputUSDPerMTok: d.OutputUSDPerMTok,
			BundleTokens:     d.BundleTokens,
		}
	}

	table, err := pricing.NewTable(ctx, pricing.TableConfig{
		MarginPercent:   cfg.Pricing.MarginPercent,
		Models:          models,
		DefaultUSD:      defaultUSD,
		DefaultModel:    cfg.DefaultModel,
		BundleTokens:    cfg.BundleTokens,
		BTCUSD:          btcUSD,
		Lister:          lister,
		RefreshInterval: cfg.RefreshInterval,
		Logger:          logger,
	})
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	// The meterd server carries the metering mechanics; our store and
	// rate table plug in through its options.
	mcfg := &meterd.Config{
		ListenAddr:             cfg.ListenAddr,
		BundleTokens:           cfg.BundleTokens,
		DefaultModel:           cfg.DefaultModel,
		EstimatedTokens:        cfg.EstimatedTokens,
		MaxUnauthorizedBundles: cfg.MaxUnauthorizedBundles,
		UnauthorizedBundleTTL:  cfg.UnauthorizedBundleTTL,
		TLSCertPath:            cfg.TLSCertPath,
		TLSKeyPath:             cfg.TLSKeyPath,
	}
	meter, err := meterd.NewServer(
		mcfg, meterd.WithStore(st), meterd.WithRateSource(table),
	)
	if err != nil {
		table.Stop()
		_ = st.Close()
		return nil, err
	}

	d := &Daemon{
		cfg:   cfg,
		log:   logger,
		store: st,
		table: table,
		meter: meter,
	}

	if cfg.Webhook.ListenAddr != "" {
		d.webhook, err = webhook.NewServer(webhook.Config{
			ListenAddr: cfg.Webhook.ListenAddr,
			Secret:     cfg.Webhook.Secret,
			Path:       cfg.Webhook.Path,
			ReconToken: cfg.Webhook.ReconToken,
			Sink:       st,
			Reporter:   st,
			Logger:     logger,
		})
		if err != nil {
			table.Stop()
			_ = st.Close()
			return nil, err
		}
	}

	return d, nil
}

// Start launches the rate table refresher, the pricesrpc server and the
// webhook listener.
func (d *Daemon) Start() error {
	d.table.Start()

	if err := d.meter.Start(); err != nil {
		return err
	}

	if d.webhook != nil {
		d.webhook.Start()
	}

	d.log.Info("lightmeter running", "listen", d.cfg.ListenAddr,
		"db", d.cfg.DBPath)

	return nil
}

// Stop shuts all services down.
func (d *Daemon) Stop() {
	if d.webhook != nil {
		d.webhook.Stop()
	}

	d.meter.Stop()
	d.table.Stop()

	if err := d.store.Close(); err != nil {
		d.log.Error("Unable to close store", "err", err)
	}
}

// Main is the true entry point of the daemon. It is factored out so that a
// thin main package can call it.
func Main() {
	cfg, err := LoadConfig(os.Args[1:])
	if err != nil {
		// The flags package already prints help output itself, so a
		// help request is not an error.
		var flagErr *flags.Error
		if errors.As(err, &flagErr) &&
			flagErr.Type == flags.ErrHelp {

			os.Exit(0)
		}

		_, _ = fmt.Fprintf(os.Stderr, "Error loading config: %v\n",
			err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), time.Minute,
	)
	daemon, err := NewDaemon(ctx, cfg)
	cancel()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error creating daemon: %v\n",
			err)
		os.Exit(1)
	}

	if err := daemon.Start(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error starting daemon: %v\n",
			err)
		os.Exit(1)
	}

	// Run until interrupted.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	slog.Info("Received shutdown signal, stopping daemon")
	daemon.Stop()
}
