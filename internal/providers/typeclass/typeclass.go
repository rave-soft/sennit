// Package typeclass classifies provider API types shared by runtime and config.
package typeclass

import "charm.land/catwalk/pkg/catwalk"

type Kind string

const (
	OpenAI       Kind = "openai"
	Anthropic    Kind = "anthropic"
	OpenRouter   Kind = "openrouter"
	Vercel       Kind = "vercel"
	Azure        Kind = "azure"
	Bedrock      Kind = "bedrock"
	Google       Kind = "google"
	GoogleVertex Kind = "google-vertex"
	OpenAICompat Kind = "openai-compat"
	Unknown      Kind = "unknown"
)

func Of(typ catwalk.Type) Kind {
	switch typ {
	case catwalk.TypeOpenAI:
		return OpenAI
	case catwalk.TypeAnthropic:
		return Anthropic
	case catwalk.TypeOpenRouter:
		return OpenRouter
	case catwalk.TypeVercel:
		return Vercel
	case catwalk.TypeAzure:
		return Azure
	case catwalk.TypeBedrock:
		return Bedrock
	case catwalk.TypeGoogle:
		return Google
	case "google-vertex":
		return GoogleVertex
	case catwalk.TypeOpenAICompat:
		return OpenAICompat
	default:
		return Unknown
	}
}
