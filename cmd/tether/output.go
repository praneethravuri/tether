package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/praneethravuri/tether/internal/sanitize"
)

// quiet marks a command as reporting its own failures -- no "Error: ..." or
// usage dump -- and makes a bad flag list this command's valid ones.
func quiet(cmd *cobra.Command) *cobra.Command {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetFlagErrorFunc(flagErrorFunc)
	return cmd
}

// flagErrorFunc appends this command's valid flags to a flag-parsing error.
func flagErrorFunc(cmd *cobra.Command, err error) error {
	return fmt.Errorf("%w\nvalid flags for %s: %s", err, cmd.Name(), flagNames(cmd))
}

// flagNames lists cmd's registered flags as "--name", in registration order.
func flagNames(cmd *cobra.Command) string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		names = append(names, "--"+f.Name)
	})
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// identityFlags are the flags shared by every command that acts as, or on
// behalf of, a particular agent.
type identityFlags struct {
	name      string
	workspace string
}

// addIdentity registers --as and --workspace on cmd.
func (f *identityFlags) addIdentity(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.name, "as", "",
		"agent name to act as (defaults to whatever this session already registered, or a minted name)")
	cmd.Flags().StringVar(&f.workspace, "workspace", "",
		"workspace to use (defaults to the shared Git-root identity of the current directory)")
}

// addWorkspace registers only --workspace, for commands that do not act as a
// specific agent.
func (f *identityFlags) addWorkspace(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.workspace, "workspace", "",
		"workspace to use (defaults to the shared Git-root identity of the current directory)")
}

// printJSON writes v as indented JSON followed by a newline. This is the only
// place JSON output is produced, so the shape stays consistent.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return failf(exitGeneral, "cannot encode the result as JSON: %v", err)
	}
	return nil
}

// sanitizeTerminal replaces C0 control bytes and DEL (except newline/tab)
// with U+FFFD so a store-derived value cannot smuggle terminal escapes into
// an error message.
func sanitizeTerminal(s string) string {
	return sanitize.Replace(s, '�', true)
}
