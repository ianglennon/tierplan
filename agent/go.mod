module github.com/ianglennon/bayplan/agent

go 1.27

require (
	github.com/coder/websocket v1.8.15
	github.com/hashicorp/yamux v0.1.2
	github.com/ianglennon/bayplan/proto v0.0.0-00010101000000-000000000000
)

replace github.com/ianglennon/bayplan/proto => ../proto
