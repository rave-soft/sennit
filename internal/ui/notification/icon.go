package notification

import _ "embed"

//go:generate go run ./genicon/cmd

// Icon is the PNG data for the Sennit notification icon: a dark rounded
// square with three crossing diagonal strands in the brand's
// primary/secondary/accent colors, evoking a plait. It's generated
// deterministically by genicon (see genicon/icon.go and the go:generate
// directive above) rather than hand-drawn. Used for both native OS
// notifications (beeep) and OSC 99 escape-sequence notifications.
//
//go:embed assets/sennit.png
var Icon []byte
