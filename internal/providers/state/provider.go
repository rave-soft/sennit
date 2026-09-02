package state

import (
	"maps"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/oauth"
)

// Provider is the effective in-memory provider state published for runtime
// consumers. Config retains the persisted source values separately.
type Provider struct {
	ID                 string
	Name               string
	BaseURL            string
	Type               catwalk.Type
	APIKey             string
	APIKeyTemplate     string
	OAuthToken         *oauth.Token
	ProxyURL           string
	ConfiguredProxyURL string
	Account            string
	ExtraHeaders       map[string]string
	ExtraParams        map[string]string
	Models             []catwalk.Model
}

func Clone(p Provider) Provider {
	p.ExtraHeaders = maps.Clone(p.ExtraHeaders)
	p.ExtraParams = maps.Clone(p.ExtraParams)
	p.Models = slices.Clone(p.Models)
	if p.OAuthToken != nil {
		token := *p.OAuthToken
		if token.Client != nil {
			client := *token.Client
			token.Client = &client
		}
		p.OAuthToken = &token
	}
	return p
}
