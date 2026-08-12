package main

// The network commands, as a shell over pkg/warren. See sandbox.go's header.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TSHOGX/warren/pkg/warren"
)

func newNetworkGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <sandbox-id>",
		Short: "Show a sandbox's network declaration",
		Long: `Show a sandbox's network declaration.

The output is a legal input to 'network patch' — that is a contract requirement, not a
convenience: editing a declaration is a read-modify-write, and a patch built from a
stale read silently drops a publish added in between, on a security-relevant field.

Read it while PAUSED. Reading before the pause pulls the whole pause window into the
window where that lost update can happen.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := api.Network(context.Background(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), n)
		},
	}
}

func newNetworkPatchCmd() *cobra.Command {
	var networkJSON string

	cmd := &cobra.Command{
		Use:   "patch <sandbox-id>",
		Short: "Replace a sandbox's network declaration",
		Long: `Replace a sandbox's network declaration, whole.

WHOLESALE IS THE CONTRACT: there is no field-level edit of a declaration, because merge
semantics on an allow-list is how a rule gets dropped without anyone deciding to drop
it. Whatever you pass here becomes the entire declaration.

The safe sequence is: pause, network get, edit, network patch, resume. Editing the
egress fields on a running sandbox is a conflict.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n warren.Network
			if err := json.Unmarshal([]byte(networkJSON), &n); err != nil {
				return fmt.Errorf("parse network JSON: %w", err)
			}
			if err := api.SetNetwork(context.Background(), args[0], n); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Network declaration replaced")
			return nil
		},
	}

	cmd.Flags().StringVar(&networkJSON, "json", "", "the complete network declaration as JSON (required)")
	_ = cmd.MarkFlagRequired("json")
	return cmd
}

func newNetworkPublishCmd() *cobra.Command {
	var (
		guestPort uint16
		proto     string
	)

	cmd := &cobra.Command{
		Use:   "publish <sandbox-id>",
		Short: "Publish a guest port",
		Long: `Publish a guest port.

Legal on a running sandbox, unlike an egress edit — the record and the kernel rules are
one un-halvable write. On a paused sandbox it is recorded and takes effect at the next
placement, which the reported 'applies_at' distinguishes; both are a success.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := api.PublishPort(context.Background(), args[0], guestPort, proto)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), res)
		},
	}

	cmd.Flags().Uint16Var(&guestPort, "guest-port", 0, "the guest port to publish (required)")
	cmd.Flags().StringVar(&proto, "proto", "tcp", "tcp or udp")
	_ = cmd.MarkFlagRequired("guest-port")
	return cmd
}
