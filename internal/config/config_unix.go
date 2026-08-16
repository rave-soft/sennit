//go:build !windows

package config

import "github.com/rave-soft/sennit/internal/brand"

// systemConfigPath is the system-wide configuration file path. It is
// loaded at the lowest priority so user and project configs override it.
const systemConfigPath = "/etc/" + brand.Slug + "/" + brand.JSONConfigFile
