//go:build !darwin

package notification

import (
	_ "embed"
)

//go:embed braid-icon-solo.png
var Icon []byte
