//go:build embed_deno

package deno

import (
	"embed"
	"fmt"
	"runtime"
)

//go:embed all:deno_embedded
var embedded embed.FS

func EmbeddedZip() []byte {
	name := fmt.Sprintf("deno_embedded/deno-%s-%s.zip", runtime.GOOS, runtime.GOARCH)
	data, err := embedded.ReadFile(name)
	if err != nil {
		return nil
	}
	return data
}
