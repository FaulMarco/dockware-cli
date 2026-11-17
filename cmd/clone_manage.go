package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/dockware/dockware-cli/clone"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	clonePurgeExpired bool
	clonePurgeYes     bool
)

func init() {
	f := clonePurgeCmd.Flags()
	f.BoolVar(&clonePurgeExpired, "expired", false, "purge all clones past their expiry date")
	f.BoolVar(&clonePurgeYes, "yes", false, "do not ask for confirmation")
	cloneCmd.AddCommand(cloneListCmd, clonePurgeCmd)
}

const cloneTimeFormat = "2006-01-02 15:04"

var cloneListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List the local clones with their source, expiry and verification state",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		clones, err := clone.ListClones()
		if err != nil {
			return err
		}
		if len(clones) == 0 {
			path, _ := clone.RegistryPath()
			fmt.Printf("no clones registered (%s)\n", path)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSOURCE\tCREATED\tEXPIRES\tVERIFIED\tOUT DIR\t")
		for _, c := range clones {
			var marks []string
			if c.Expired {
				marks = append(marks, "EXPIRED")
			}
			if !c.Exists {
				marks = append(marks, "missing")
			}
			verified := "no"
			switch {
			case c.Wiped:
				verified = "wiped"
			case c.Verified:
				verified = "yes"
			}
			fmt.Fprintf(w, "%s\t%s:%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Target, c.Root, c.Time.Local().Format(cloneTimeFormat),
				c.ExpiresAt.Local().Format(cloneTimeFormat), verified, c.OutDir, strings.Join(marks, " "))
		}
		return w.Flush()
	},
}

var clonePurgeCmd = &cobra.Command{
	Use:   "purge [name...]",
	Short: "Remove clones: containers, database volumes and the files they wrote",
	Long: `Purge removes clones: docker compose down -v for their stack (containers
and the cloned database) and the files the clone wrote into its output
directory (synced code, dump, compose and env files, logs). The directory
itself is removed when nothing else is left in it. Name the clones (name or
output directory, see 'dockware-cli clone list') or pass --expired for all clones
past their expiry date. An output directory missing from the registry is
accepted when it holds the clone's clone.log.json and docker-compose.yml.`,
	Example: `  dockware-cli clone purge --expired
  dockware-cli clone purge dwc-shop-stage-3f9a1c --yes
  dockware-cli clone purge ./dwc-shop-stage`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && !clonePurgeExpired {
			return fmt.Errorf("name the clones to purge or pass --expired (see 'dockware-cli clone list')")
		}
		clones, err := clone.ListClones()
		if err != nil {
			return err
		}
		selected, err := selectClones(clones, args, clonePurgeExpired)
		if err != nil {
			return err
		}
		if len(selected) == 0 {
			fmt.Println("nothing to purge")
			return nil
		}
		for _, c := range selected {
			fmt.Printf("  %s  %s:%s  %s\n", c.Name, c.Target, c.Root, c.OutDir)
		}
		if !clonePurgeYes {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("confirmation needs an interactive terminal; pass --yes")
			}
			if !askYesNo(fmt.Sprintf("Delete these %d clones with their data?", len(selected))) {
				return nil
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		failed := 0
		for _, c := range selected {
			if err := clone.Purge(ctx, c, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", c.Name, err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d clones could not be purged", failed, len(selected))
		}
		return nil
	},
}

// selectClones picks the clones named by name or output directory, plus all
// expired ones with expired; an output directory missing from the registry
// is taken from its own files, any other unknown name is an error.
func selectClones(clones []clone.CloneInfo, names []string, expired bool) ([]clone.CloneInfo, error) {
	var out []clone.CloneInfo
	seen := map[string]bool{}
	add := func(c clone.CloneInfo) {
		if !seen[c.OutDir] {
			seen[c.OutDir] = true
			out = append(out, c)
		}
	}
	for _, n := range names {
		dir, _ := filepath.Abs(n)
		found := false
		for _, c := range clones {
			if c.Name == n || c.OutDir == dir {
				add(c)
				found = true
			}
		}
		if found {
			continue
		}
		c, err := clone.UnregisteredClone(n)
		if err != nil {
			return nil, fmt.Errorf("no registered clone %q (see 'dockware-cli clone list') and no clone output directory: %v", n, err)
		}
		add(c)
	}
	if expired {
		for _, c := range clones {
			if c.Expired {
				add(c)
			}
		}
	}
	return out, nil
}
