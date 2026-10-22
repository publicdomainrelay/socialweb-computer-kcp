package deploy

import "embed"

//go:embed *.yaml apps/*.yaml apps/*/*.yaml eso/*.yaml market-network/*.yaml
var FS embed.FS
