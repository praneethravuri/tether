package main

import (
	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

// runRoot is root's RunE for a bare `tether` invocation: a JSON summary
// of the current workspace state.
func runRoot(cmd *cobra.Command) error {
	workspace, err := resolveWorkspace("")
	if err != nil {
		return err
	}

	var ls protocol.LsResult
	if err := call(protocol.MethodLs, protocol.LsParams{Workspace: workspace}, &ls); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if ls.Agents == nil {
		ls.Agents = []protocol.AgentView{}
	}
	return printJSON(out, ls)
}
