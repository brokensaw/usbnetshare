//go:build windows && drivers

package drivers

import "embed"

//go:embed payload
var payload embed.FS

func init() { bundle = payload }
