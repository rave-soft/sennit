package cmd

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/spf13/cobra"
)

var (
	importSkillsFlag bool
	importAgentsFlag bool
	importDryRun     bool
	importGlobal     bool
	importForce      bool
)

var importCmd = &cobra.Command{
	Use:   "import claude|opencode",
	Short: "Import skills and/or agents from Claude Code or opencode",
	Long: `Sennit only auto-discovers its own skills (.sennit/skills) and agents
(.sennit/agents) — see TECHDEBT.md. This command is the supported way to bring
in a role or skill written for another tool: it copies from that tool's
directories into Sennit's own, validating and translating fields as it goes
(model, reasoning_effort/effort, tools) rather than trusting the foreign
directory implicitly.

Neither --skills nor --agents is on by default; pass at least one. Files
that already exist at the destination are left alone unless --force is
given. --dry-run reports what would happen without writing anything.

Both spellings of opencode's directories are read, since opencode reads
both itself: skill/ and skills/, agent/ and agents/. A name found in more
than one is imported once and reported as skipped against the rest. When
nothing is found, the directories that were searched are listed.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var source config.ImportSource
		switch strings.ToLower(args[0]) {
		case string(config.ImportSourceClaude):
			source = config.ImportSourceClaude
		case string(config.ImportSourceOpenCode):
			source = config.ImportSourceOpenCode
		default:
			return fmt.Errorf("unknown import source %q (want %q or %q)", args[0], config.ImportSourceClaude, config.ImportSourceOpenCode)
		}

		if !importSkillsFlag && !importAgentsFlag {
			return fmt.Errorf("nothing to import: pass --skills and/or --agents")
		}

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

		report, err := config.RunImport(config.ImportOptions{
			Source:     source,
			WorkingDir: cwd,
			Providers:  cfg.Config().Providers.Copy(),
			Skills:     importSkillsFlag,
			Agents:     importAgentsFlag,
			DryRun:     importDryRun,
			Global:     importGlobal,
			Force:      importForce,
		})
		if err != nil {
			return err
		}

		printImportReport(cmd, report, importDryRun)
		return nil
	},
}

// printImportReport renders the import outcome as a table: kind, name,
// status, and the reason/warnings behind it.
func printImportReport(cmd *cobra.Command, report config.ImportReport, dryRun bool) {
	if len(report.Entries) == 0 {
		// Name the directories. "Nothing to import" on its own reads as
		// a fact about the user's setup when it may be a fact about
		// where this build looked — and the two call for opposite next
		// moves.
		cmd.Printf("Nothing to import from %s. Looked in:\n", report.Source)
		for _, dir := range report.Searched {
			cmd.Printf("  %s\n", home.Short(dir))
		}
		return
	}

	if dryRun {
		cmd.Println("Dry run — nothing was written.")
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tNAME\tSTATUS\tDETAILS")
	for _, e := range report.Entries {
		details := e.Reason
		if len(e.Warnings) > 0 {
			details = strings.Join(e.Warnings, "; ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Kind, e.Name, e.Status, details)
	}
	_ = w.Flush()

	counts := map[config.ImportStatus]int{}
	for _, e := range report.Entries {
		counts[e.Status]++
	}
	kinds := make([]string, 0, len(counts))
	for status := range counts {
		kinds = append(kinds, string(status))
	}
	sort.Strings(kinds)
	cmd.Printf("%d imported, %d adjusted, %d skipped\n",
		counts[config.StatusImported], counts[config.StatusAdjusted], counts[config.StatusSkipped])
}

func init() {
	importCmd.Flags().BoolVar(&importSkillsFlag, "skills", false, "import skills")
	importCmd.Flags().BoolVar(&importAgentsFlag, "agents", false, "import agents")
	importCmd.Flags().BoolVar(&importDryRun, "dry-run", false, "report what would happen without writing anything")
	importCmd.Flags().BoolVar(&importGlobal, "global", false, "read/write the user-level directories instead of the project ones")
	importCmd.Flags().BoolVar(&importForce, "force", false, "overwrite files that already exist at the destination")
	rootCmd.AddCommand(importCmd)
}
