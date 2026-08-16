// Package brand centralizes every brand-derived identifier Sennit uses:
// display name, binary/directory/file name stem, environment variable
// prefix, config file names, state file names, and tool names. It has no
// imports so any package can depend on it without risking an import cycle.
//
// A future rename becomes an edit to the three root constants below instead
// of a repo-wide search-and-replace.
package brand

// Name is the display name used in UI text and prose.
const Name = "Sennit"

// Slug is the lowercase identifier used as the binary name, the directory
// and file name stem, and the tool name prefix.
const Slug = "sennit"

// EnvName is the bare, unprefixed environment variable name Sennit sets to
// mark its own shell sessions (e.g. SENNIT=1). It cannot be derived from
// Slug at compile time (case differs), so it is its own root constant.
const EnvName = "SENNIT"

// EnvPrefix is the prefix of every environment variable the product owns.
const EnvPrefix = EnvName + "_"

// Vendor is the GitHub organization / vendor slug that owns the product. It
// is not derived from Name or Slug and changes independently of them, so it
// is its own root constant.
const Vendor = "rave-soft"

// RepoURL is the canonical repository URL.
const RepoURL = "https://github.com/" + Vendor + "/" + Slug

// Wordmark is the all-caps display form of the name, used by the header
// logo. It happens to share its spelling with EnvName today, but the two are
// separate contracts (display string vs. environment variable name) and must
// not be collapsed into one constant.
const Wordmark = "SENNIT"

// Config discovery: the project-local data directory and the config file
// names users create by hand or that Sennit writes to.
const (
	// DataDir is the project-local data/config subdirectory Sennit walks up
	// to when discovering a project.
	DataDir = "." + Slug

	// ShellConfigFile is the bare sennitrc file name, Sennit's primary Bash
	// config format.
	ShellConfigFile = Slug + "rc"

	// HiddenShellConfigFile is the dotfile variant of ShellConfigFile.
	HiddenShellConfigFile = "." + ShellConfigFile

	// JSONConfigFile is the bare sennit.json file name, a deprecated config
	// format still supported for compatibility.
	JSONConfigFile = Slug + ".json"

	// HiddenJSONConfigFile is the dotfile variant of JSONConfigFile.
	HiddenJSONConfigFile = "." + JSONConfigFile

	// IgnoreFile is the file users create to exclude paths from Sennit,
	// analogous to .gitignore.
	IgnoreFile = "." + Slug + "ignore"

	// ContextFile is the uppercase project context file Sennit reads
	// automatically. Casing and .local variants of this name are enumerated
	// by the caller, not derived here.
	ContextFile = "SENNIT.md"

	// ContextFileLocal is the .local sibling of ContextFile.
	ContextFileLocal = "SENNIT.local.md"
)

// State file names: files Sennit itself creates and manages under its data
// directory.
const (
	// DBFile is the SQLite database file name.
	DBFile = Slug + ".db"

	// LogFile is the default log file name.
	LogFile = Slug + ".log"

	// LockFile is the process lock file name.
	LockFile = Slug + ".lock"
)

// Tools and URIs: identifiers models and skill authors see.
const (
	// SkillsURIScheme is the URI scheme prefix used to reference bundled
	// skills.
	SkillsURIScheme = Slug + "://"

	// ToolInfo is the name of the built-in "info" tool, as seen by models.
	ToolInfo = Slug + "_info"

	// ToolLogs is the name of the built-in "logs" tool, as seen by models.
	ToolLogs = Slug + "_logs"

	// IconFile is the notification icon asset written into the cache
	// directory.
	IconFile = Slug + ".png"
)
