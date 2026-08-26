package accounts

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/proxyhttp"
)

func TestResolveProxy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		accountProxy  string
		providerProxy string
		want          string
	}{
		{
			name:          "account overrides provider",
			accountProxy:  "http://account:8080",
			providerProxy: "http://provider:8080",
			want:          "http://account:8080",
		},
		{
			name:          "empty account inherits provider",
			accountProxy:  "",
			providerProxy: "http://provider:8080",
			want:          "http://provider:8080",
		},
		{
			name:          "both empty yields empty",
			accountProxy:  "",
			providerProxy: "",
			want:          "",
		},
		{
			name:          "account none wins over provider url",
			accountProxy:  proxyhttp.Direct,
			providerProxy: "http://provider:8080",
			want:          proxyhttp.Direct,
		},
		{
			name:          "provider none wins when account empty",
			accountProxy:  "",
			providerProxy: proxyhttp.Direct,
			want:          proxyhttp.Direct,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ResolveProxy(tt.accountProxy, tt.providerProxy))
		})
	}
}

func TestIsDirect(t *testing.T) {
	t.Parallel()

	require.True(t, IsDirect(proxyhttp.Direct))
	require.False(t, IsDirect(""))
	require.False(t, IsDirect("http://proxy:8080"))
}
