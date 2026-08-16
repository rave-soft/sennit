package cmd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/spf13/cobra"
)

var doctorJSON bool

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the loaded configuration for problems",
	Long: `Check the loaded configuration for problems: agents pinned to a
model that doesn't resolve to a provider, a reasoning_effort set on a model
that can't reason, providers dropped for a missing api key, a main model
that fell back to a default, and disabled_tools/allowed_tools typos.

This only inspects the config as loaded; it does not make network calls to
verify a provider's api key or endpoint actually works (use 'sennit models
refresh' or the TUI's "Test Connection" for that). MCP server connection
health is likewise only checked from a running session, since this command
never starts MCP servers itself; see the TUI's /doctor command or the
sennit_info tool for that.

Exit code is 1 if any problem is severity "error", 0 otherwise (including
when only warnings were found).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		dataDir, _ := cmd.Flags().GetString("data-dir")
		debug, _ := cmd.Flags().GetBool("debug")

		cfg, err := config.Init(cwd, dataDir, debug)
		if err != nil {
			return err
		}

		problems := config.Doctor(cfg.Config())

		if doctorJSON {
			bts, err := json.MarshalIndent(problems, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal problems: %w", err)
			}
			cmd.Println(string(bts))
		} else {
			printDoctorProblems(cmd, problems)
		}

		errCount := 0
		for _, p := range problems {
			if p.Severity == config.SeverityError {
				errCount++
			}
		}
		if errCount > 0 {
			return fmt.Errorf("%d config problem(s), %d blocking", len(problems), errCount)
		}
		return nil
	},
}

// printDoctorProblems renders problems grouped by area, in a stable order,
// for human consumption on a terminal.
func printDoctorProblems(cmd *cobra.Command, problems []config.Problem) {
	if len(problems) == 0 {
		cmd.Println("No config problems found.")
		return
	}

	sorted := slices.Clone(problems)
	slices.SortFunc(sorted, func(a, b config.Problem) int {
		if c := cmp.Compare(a.Area, b.Area); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Subject, b.Subject); c != 0 {
			return c
		}
		return cmp.Compare(a.Message, b.Message)
	})

	currentArea := config.Area("")
	for _, p := range sorted {
		if p.Area != currentArea {
			cmd.Printf("[%s]\n", p.Area)
			currentArea = p.Area
		}
		cmd.Printf("  %s: %s\n", p.Severity, p.Message)
		if p.Hint != "" {
			cmd.Printf("    hint: %s\n", p.Hint)
		}
	}
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "output problems as JSON")
	rootCmd.AddCommand(doctorCmd)
}
