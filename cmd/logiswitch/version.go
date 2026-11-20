package main

// version is set at build time by the release workflow:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/logiswitch
var version = "dev"
