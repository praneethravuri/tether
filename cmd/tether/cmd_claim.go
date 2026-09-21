package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

const claimLong = `Claim exclusive ownership of a key (typically a file path) within a workspace.

A claim belongs to the calling process, not to any registered agent name --
identified by pid and start time, the same self-healing pairing tether uses
for presence. Running this again from the same live process renews the
claim and mints a fresh lease id; a claim whose process has died can be
reclaimed by anyone immediately, without waiting for its TTL to elapse.

The returned lease id is required to release the claim (--if-claim-id) --
keep it.`

type claimOptions struct {
	identityFlags
	holder string
}

func newClaimCmd() *cobra.Command {
	var opts claimOptions

	cmd := &cobra.Command{
		Use:   "claim <key>",
		Short: "Claim exclusive ownership of a key in this workspace",
		Long:  claimLong,
		Example: "  tether claim src/main.go\n" +
			"  tether claim src/main.go --holder \"refactoring auth\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaim(cmd, args[0], &opts)
		},
	}

	opts.addWorkspace(cmd)
	cmd.Flags().StringVar(&opts.holder, "holder", "",
		"free-text label shown by tether claims (e.g. \"refactoring auth\")")

	return quiet(cmd)
}

func runClaim(cmd *cobra.Command, key string, opts *claimOptions) error {
	ws, err := resolveWorkspace(opts.workspace)
	if err != nil {
		return err
	}

	params := protocol.ClaimParams{
		Workspace: ws,
		Key:       key,
		OwnerPID:  os.Getppid(), // the shell, not this short-lived CLI process
		Holder:    opts.holder,
	}

	var res protocol.ClaimResult
	if err := call(protocol.MethodClaim, params, &res); err != nil {
		return claimError(key, ws, err)
	}

	out := cmd.OutOrStdout()
	return printJSON(out, res)
}

// claimError turns a 409 (key held by a live owner) into an actionable
// message.
func claimError(key, workspace string, err error) error {
	if code, ok := daemonCode(err); ok && code == protocol.CodeConflict {
		var pe *protocol.Error
		_ = errors.As(err, &pe)
		return fail(exitConflict, fmt.Errorf(
			"cannot claim %s in %s: %s\n"+
				"       the key is held by a live process — run `tether claims` to see who, "+
				"or wait for it to be released",
			key, workspace, sanitizeTerminal(pe.Message)))
	}
	return err
}
