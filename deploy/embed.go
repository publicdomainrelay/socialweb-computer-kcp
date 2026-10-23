package deploy

import "embed"

//go:embed *.yaml apps/*.yaml apps/*/*.yaml eso/*.yaml market-network/*.yaml
var FS embed.FS

//go:embed bus-bootstrap-callers.txt
var BusBootstrapCallers string
