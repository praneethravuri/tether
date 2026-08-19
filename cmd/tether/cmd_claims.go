package main

import (
	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

const claimsLong = `List claims in this workspace: who holds what, and whether it's still live.
Use --all to list every workspace; it ignores --workspace.

STATUS is computed fresh on every call: held (owner alive, TTL not elapsed),
expired (TTL elapsed), or gone (owner process no longer alive) -- the same
self-healing check ` + "`tether ls`" + ` uses for agent presence.

Output is JSON by default.`

type claimsOptions struct {
	identityFlags
	all bool
}

func newClaimsCmd() *cobra.Command {
	var opts claimsOptions

	cmd := &cobra.Command{
		Use:   "claims",
		Short: "List claims and who holds them",
		Long:  claimsLong,
		Example: "  tether claims\n" +
			"  tether claims --all",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClaims(cmd, &opts)
		},
	}

	opts.addWorkspace(cmd)
	cmd.Flags().BoolVar(&opts.all, "all", false, "list claims in every workspace (ignores --workspace)")

	return quiet(cmd)
}

func runClaims(cmd *cobra.Command, opts *claimsOptions) error {
	ws, err := fleetWorkspace(opts.workspace, opts.all)
	if err != nil {
		return err
	}

	var res protocol.ClaimsResult
	if err := call(protocol.MethodClaims, protocol.ClaimsParams{Workspace: ws}, &res); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if res.Claims == nil {
		res.Claims = []protocol.ClaimView{}
	}
	return printJSON(out, res)
}
