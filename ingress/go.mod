module github.com/ianglennon/tierplan/ingress

go 1.27

require (
	github.com/coder/websocket v1.8.15
	github.com/ianglennon/tierplan/proto v0.0.0-00010101000000-000000000000
	go.yaml.in/yaml/v3 v3.0.5
)

replace github.com/ianglennon/tierplan/proto => ../proto
