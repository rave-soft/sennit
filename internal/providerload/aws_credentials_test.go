package providerload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
)

// TestHasAWSCredentials covers the Bedrock credential probe on the path
// production actually takes.
//
// It used to live in internal/config, against a byte-for-byte duplicate
// named hasAWSCredentialsWithFiles that no production code called — so
// the copy this loader resolves Bedrock with had no unit test at all,
// while the unused one did. The duplicate is gone; the coverage moved
// here rather than being deleted with it.
func TestHasAWSCredentials(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		env       map[string]string
		files     map[string]bool
		wantPaths []string
	}{
		{name: "environment", env: map[string]string{"AWS_ACCESS_KEY_ID": "key", "AWS_SECRET_ACCESS_KEY": "secret"}},
		{name: "credentials file", files: map[string]bool{".aws/credentials": true}, wantPaths: []string{".aws/credentials"}},
		{name: "login file", files: map[string]bool{".aws/login": true}, wantPaths: []string{".aws/credentials", ".aws/login"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var paths []string
			stat := func(path string) (os.FileInfo, error) {
				paths = append(paths, path)
				rel, err := filepath.Rel("/isolated/home", path)
				require.NoError(t, err)
				// The table keys are slash paths; filepath.Rel hands back
				// backslashes on Windows, where every lookup missed and
				// the credentials went undetected.
				if test.files[filepath.ToSlash(rel)] {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}

			require.True(t, hasAWSCredentials(testenv.New(test.env), "/isolated/home", stat))
			var wantPaths []string
			if len(test.wantPaths) > 0 {
				wantPaths = make([]string, len(test.wantPaths))
			}
			for i, path := range test.wantPaths {
				wantPaths[i] = filepath.Join("/isolated/home", path)
			}
			require.Equal(t, wantPaths, paths)
		})
	}
}

// TestHasAWSCredentials_NoneReportsFalse pins the negative case the moved
// table did not cover: no environment, no files, no credentials.
func TestHasAWSCredentials_NoneReportsFalse(t *testing.T) {
	t.Parallel()

	stat := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	require.False(t, hasAWSCredentials(testenv.New(nil), "/isolated/home", stat))
}
