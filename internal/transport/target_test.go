package transport

import "testing"

func TestParseTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    Target
		wantErr bool
	}{
		{
			name: "user host port path",
			raw:  "ssh://alice@example.com:2222/home/alice/project",
			want: Target{User: "alice", Host: "example.com", Port: "2222", Path: "/home/alice/project"},
		},
		{
			name: "no user no port",
			raw:  "ssh://example.com/home/alice/project",
			want: Target{Host: "example.com", Path: "/home/alice/project"},
		},
		{
			name:    "wrong scheme",
			raw:     "https://example.com/home/alice/project",
			wantErr: true,
		},
		{
			name:    "no path at all",
			raw:     "ssh://example.com",
			wantErr: true,
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "no host",
			raw:     "ssh:///abs/path",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTarget(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTarget(%q): expected error, got %+v", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseTarget(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestTargetString_RoundTrips(t *testing.T) {
	t.Parallel()
	raw := "ssh://alice@example.com:2222/home/alice/project"
	target, err := ParseTarget(raw)
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	if got := target.String(); got != raw {
		t.Fatalf("String() = %q, want %q", got, raw)
	}
}
