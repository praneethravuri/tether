package main

import (
	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

const lsLong = `List the agents registered with the daemon, and what each one is doing.

Only this workspace is shown unless --all is given; --all ignores
--workspace. NAME is the full address: copy it straight into ` + "`tether send`" + `.

STATE is computed fresh on every call and includes the evidence behind each
agent's state.

Output is JSON by default.`

type lsOptions struct {
	identityFlags
	all bool
}

func newLsCmd() *cobra.Command {
	var opts lsOptions

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List registered agents and what each is doing",
		Long:  lsLong,
		Example: "  tether ls\n" +
			"  tether ls --all",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLs(cmd, &opts)
		},
	}

	opts.addWorkspace(cmd)
	cmd.Flags().BoolVar(&opts.all, "all", false, "list agents in every workspace (ignores --workspace)")

	return quiet(cmd)
}

func runLs(cmd *cobra.Command, opts *lsOptions) error {
	workspace, err := fleetWorkspace(opts.workspace, opts.all)
	if err != nil {
		return err
	}

	var res protocol.LsResult
	if err := call(protocol.MethodLs,
		protocol.LsParams{Workspace: workspace}, &res); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if res.Agents == nil {
		res.Agents = []protocol.AgentView{}
	}
	return printJSON(out, res)
}

// fleetWorkspace resolves ls's workspace: every workspace when all is set,
// otherwise the usual flag/cwd resolution.
func fleetWorkspace(workspaceFlag string, all bool) (string, error) {
	if all {
		return "", nil
	}
	return resolveWorkspace(workspaceFlag)
}
