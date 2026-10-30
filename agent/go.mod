module github.com/ianglennon/coaming/agent

go 1.27

require (
	github.com/coder/websocket v1.8.15
	github.com/containerd/errdefs v1.0.0
	github.com/distribution/reference v0.6.0
	github.com/hashicorp/yamux v0.1.2
	github.com/ianglennon/coaming/proto v0.0.0-00010101000000-000000000000
	github.com/moby/docker-image-spec v1.3.1
	github.com/moby/moby/api v1.56.0
	github.com/moby/moby/client v0.6.0
	github.com/opencontainers/image-spec v1.1.1
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.60.0 // indirect
	go.opentelemetry.io/otel v1.35.0 // indirect
	go.opentelemetry.io/otel/metric v1.35.0 // indirect
	go.opentelemetry.io/otel/trace v1.35.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
)

replace github.com/ianglennon/coaming/proto => ../proto
