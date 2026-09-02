package agent

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

func TestExtractAWSSSOURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "standard aws sso login output",
			output: `If the browser does not open or you wish to use a different device to authorize this request, open the following URL:
https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH`,
			want: "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH",
		},
		{
			name:   "url only",
			output: "https://device.sso.eu-west-1.amazonaws.com/?user_code=XXXX-YYYY",
			want:   "https://device.sso.eu-west-1.amazonaws.com/?user_code=XXXX-YYYY",
		},
		{
			name:   "no url",
			output: "SSO session expired. Please run aws sso login.",
			want:   "",
		},
		{
			name:   "empty output",
			output: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := extractAWSSSOURL(tt.output); got != tt.want {
				t.Errorf("extractAWSSSOURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// refreshAWSCredentials / runAWSAuthRefresh
// ---------------------------------------------------------------------------

func TestRefreshAWSCredentials_Headless_ReturnsErrNoInteractiveAuth(t *testing.T) {
	t.Parallel()
	c := &runtimeBuilder{agentDeps: &agentDeps{}}
	err := c.refreshAWSCredentials(t.Context(), config.ProviderConfig{ID: "bedrock", AWSAuthRefresh: "aws sso login"}, runtimeOperationPort{})
	require.ErrorIs(t, err, errNoInteractiveAuth,
		"with no notifier available, refreshAWSCredentials must not attempt to run the refresh command")
}

// TestRefreshAWSCredentials_Success_PublishesURLAndRetries covers the happy
// path: the refresh command prints its verification URL, exits zero, and
// refreshAWSCredentials must publish the two-part AWS SSO dialog
// (open, then URL fill-in), publish a success result, and return nil so
// the caller retries the request.
func TestRefreshAWSCredentials_Success_PublishesURLAndRetries(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t, withNotify(notifier))
	const ssoURL = "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH"
	providerCfg := config.ProviderConfig{ID: "bedrock", AWSAuthRefresh: "echo " + ssoURL}

	logs := captureLogs(t)
	ctx := WithRunID(context.WithValue(t.Context(), tools.SessionIDContextKey, "aws-session"), "aws-run")
	err := co.builder.refreshAWSCredentials(ctx, providerCfg, runtimeOperationPort{})
	require.NoError(t, err)
	require.Contains(t, logs.String(), "event=invalidate")
	require.Contains(t, logs.String(), "reason=aws_auth_refresh")
	require.Contains(t, logs.String(), "session_id=aws-session")
	require.Contains(t, logs.String(), "run_id=aws-run")

	dialogs := notifier.ofType(notify.TypeAWSSSOAuth)
	require.GreaterOrEqual(t, len(dialogs), 2, "expected the initial dialog-open plus a follow-up carrying the URL")
	require.Empty(t, dialogs[0].AWSSOURL, "the first publish opens the dialog before any URL is known")
	var sawURL bool
	for _, d := range dialogs {
		if d.AWSSOURL == ssoURL {
			sawURL = true
		}
	}
	require.True(t, sawURL, "the verification URL scraped from the command's output must be published")

	results := notifier.ofType(notify.TypeAWSSSOAuthResult)
	require.Len(t, results, 1)
	require.Empty(t, results[0].Message, "a successful refresh command must publish an empty-message result")
}

// TestRefreshAWSCredentials_CommandFailure_ReturnsErrorWithStderr covers a
// refresh command that fails: refreshAWSCredentials must still publish a
// result notification (this time carrying the error text) and return the
// command's error instead of nil.
func TestRefreshAWSCredentials_CommandFailure_ReturnsErrorWithStderr(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t, withNotify(notifier))
	providerCfg := config.ProviderConfig{ID: "bedrock", AWSAuthRefresh: "echo login-failed-detail 1>&2; exit 7"}

	err := co.builder.refreshAWSCredentials(t.Context(), providerCfg, runtimeOperationPort{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "login-failed-detail")

	results := notifier.ofType(notify.TypeAWSSSOAuthResult)
	require.Len(t, results, 1)
	require.Contains(t, results[0].Message, "login-failed-detail")
}

// TestRefreshAWSCredentials_CallerCancelled_ReturnsCallerCtxErr covers the
// case where the refresh command itself succeeds (it runs on a context
// detached from the caller, see the comment on refreshAWSCredentials) but
// the caller's context was cancelled while it ran: the failed request must
// not be retried, so refreshAWSCredentials must surface the caller's
// cancellation instead of nil.
func TestRefreshAWSCredentials_CallerCancelled_ReturnsCallerCtxErr(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t, withNotify(notifier))
	providerCfg := config.ProviderConfig{ID: "bedrock", AWSAuthRefresh: "true"}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := co.builder.refreshAWSCredentials(ctx, providerCfg, runtimeOperationPort{})
	require.ErrorIs(t, err, context.Canceled)
}

// TestRunAWSAuthRefresh_PublishesFirstURLOnly covers runAWSAuthRefresh
// directly: when both stdout and stderr carry a candidate verification
// URL, only the first one scraped is published (urlSent dedup), and the
// command's own success/failure is reported accurately.
func TestRunAWSAuthRefresh_PublishesFirstURLOnly(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t, withNotify(notifier))
	providerCfg := config.ProviderConfig{
		ID: "bedrock",
		AWSAuthRefresh: "echo https://sso.example.com/first >&1; " +
			"echo https://sso.example.com/second >&2",
	}

	err := co.builder.runAWSAuthRefresh(t.Context(), providerCfg)
	require.NoError(t, err)

	dialogs := notifier.ofType(notify.TypeAWSSSOAuth)
	var urls []string
	for _, d := range dialogs {
		if d.AWSSOURL != "" {
			urls = append(urls, d.AWSSOURL)
		}
	}
	require.Len(t, urls, 1, "only the first verification URL scraped from the command output must be published")
}

// TestRunAWSAuthRefresh_NonZeroExit_ReturnsErrorWithStderr covers a
// command that exits non-zero with no URL in its output: the returned
// error must fold in the captured stderr for operator context.
func TestRunAWSAuthRefresh_NonZeroExit_ReturnsErrorWithStderr(t *testing.T) {
	co := authTestCoordinator(t, withNotify(&recordingNotifier{}))
	providerCfg := config.ProviderConfig{ID: "bedrock", AWSAuthRefresh: "echo boom 1>&2; exit 3"}

	err := co.builder.runAWSAuthRefresh(t.Context(), providerCfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

func TestRunAWSAuthRefresh_OversizedLine_ReturnsScannerError(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
	}{
		{name: "stdout", command: "python3 -c 'print(\"x\" * 1048577)'"},
		{name: "stderr", command: "python3 -c 'import sys; print(\"x\" * 1048577, file=sys.stderr)'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			co := authTestCoordinator(t, withNotify(&recordingNotifier{}))
			err := co.builder.runAWSAuthRefresh(t.Context(), config.ProviderConfig{ID: "bedrock", AWSAuthRefresh: tt.command})
			require.Error(t, err)
			require.Contains(t, err.Error(), "token too long")
		})
	}
}
