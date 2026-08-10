package chat

import (
	"strings"

	"github.com/rave-soft/braid/internal/stringext"
)

func humanizedToolName(name string) string {
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")
	return stringext.Capitalize(name)
}
