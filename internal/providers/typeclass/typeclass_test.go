package typeclass

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestOfClassifiesEverySupportedType(t *testing.T) {
	tests := map[catwalk.Type]Kind{
		catwalk.TypeOpenAI: OpenAI, catwalk.TypeAnthropic: Anthropic,
		catwalk.TypeOpenRouter: OpenRouter, catwalk.TypeVercel: Vercel,
		catwalk.TypeAzure: Azure, catwalk.TypeBedrock: Bedrock,
		catwalk.TypeGoogle: Google, "google-vertex": GoogleVertex,
		catwalk.TypeOpenAICompat: OpenAICompat, "unknown": Unknown,
	}
	for typ, want := range tests {
		t.Run(string(typ), func(t *testing.T) { require.Equal(t, want, Of(typ)) })
	}
}
