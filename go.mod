module github.com/melodic-software/ci-runner

go 1.26.5

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/actions/scaleset v0.4.0
	github.com/containerd/errdefs v1.0.0
	github.com/google/uuid v1.6.0
	github.com/hashicorp/go-retryablehttp v0.7.8
	github.com/moby/moby/api v1.55.0
	github.com/moby/moby/client v0.5.0
	github.com/opencontainers/image-spec v1.1.1
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/sys v0.47.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	// Keep the Scale Set Client's OpenTelemetry family on one reviewed, patched release (GHSA-mh2q-q3fh-2475).
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.65.0 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
)
