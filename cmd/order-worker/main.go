// Command order-worker is one of the demo services the release controller deploys.
package main

import "github.com/Abhayrkhot/release-orchestrator/internal/demosvc"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() { demosvc.Main(demosvc.RoleWorker, version) }
