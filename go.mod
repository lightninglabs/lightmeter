module github.com/lightninglabs/lightmeter

go 1.26.1

// Mirror aperture's own replace directives, which do not apply
// transitively to importers.
replace google.golang.org/protobuf => github.com/lightninglabs/protobuf-go-hex-display v1.34.2-hex-display

replace github.com/lightninglabs/lndclient => github.com/ellemouton/lndclient v0.0.0-20260623222957-e75f91198cdc

require (
	github.com/goccy/go-yaml v1.15.23
	github.com/jessevdk/go-flags v1.6.1
	github.com/lightninglabs/aperture v0.5.1-0.20261006235528-5365188b0696
	github.com/stretchr/testify v1.11.1
	modernc.org/sqlite v1.29.10
)

require (
	github.com/btcsuite/btclog v1.0.0 // indirect
	github.com/btcsuite/btclog/v2 v2.0.1-0.20250728225537-6090e87c6c5b // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.49.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)
