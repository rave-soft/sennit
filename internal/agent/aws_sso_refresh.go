package agent

import (
	"regexp"
	"time"
)

// awsSSORefreshTimeout bounds how long the AWS SSO refresh command may run.
// Browser-based SSO needs time, so it is generous, and it runs on a context
// detached from the agent turn so a cancelled turn doesn't abort the login.
const awsSSORefreshTimeout = 5 * time.Minute

// awsSSOURLRe matches the https verification URL that `aws sso login` and
// related commands print to stdout or stderr.
var awsSSOURLRe = regexp.MustCompile(`https://[^\s]+`)

const awsSSOOutputLineLimit = 1024 * 1024

// extractAWSSSOURL returns the first HTTPS URL in the given command output
// line, or empty if none is present.
func extractAWSSSOURL(line string) string {
	return awsSSOURLRe.FindString(line)
}
