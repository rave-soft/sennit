package accounts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNextID_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		existing  []Account
		accountID string
		want      string
	}{
		{
			name:      "normalizes case and garbage characters",
			accountID: "Chatgpt-Account@123!",
			want:      "acc_chatgpt-account-123",
		},
		{
			name:      "collision with existing gets a numeric suffix",
			existing:  []Account{{ID: "acc_dup"}},
			accountID: "dup",
			want:      "acc_dup-2",
		},
		{
			name:      "double collision keeps incrementing",
			existing:  []Account{{ID: "acc_dup"}, {ID: "acc_dup-2"}},
			accountID: "dup",
			want:      "acc_dup-3",
		},
		{
			name:      "empty account id starts numeric sequence at 1",
			accountID: "",
			want:      "acc_1",
		},
		{
			name:      "empty account id fills a gap in the numeric sequence",
			existing:  []Account{{ID: "acc_1"}, {ID: "acc_3"}},
			accountID: "",
			want:      "acc_2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NextID(tt.existing, tt.accountID)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNextID_TruncatesLongAccountID(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 100)
	got := NextID(nil, long)
	require.LessOrEqual(t, len(got), len(idPrefix)+maxNormalizedIDLen)
	require.Equal(t, idPrefix+long[:maxNormalizedIDLen], got)
}

func TestNextID_AllGarbageFallsBackToNumeric(t *testing.T) {
	t.Parallel()
	got := NextID(nil, "@@@###")
	require.Equal(t, "acc_1", got)
}

func TestNextID_Deterministic(t *testing.T) {
	t.Parallel()
	existing := []Account{{ID: "acc_a"}, {ID: "acc_1"}}
	first := NextID(existing, "Some-Account")
	second := NextID(existing, "Some-Account")
	require.Equal(t, first, second)
}
