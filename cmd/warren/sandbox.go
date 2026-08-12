package main

// The sandbox commands, as a SHELL over pkg/warren (ADR-0008 part 2).
//
// This file used to hold its own copies of the §6.1 wire types plus a hand-rolled
// request/decode/status-check cycle per command — a THIRD statement of the same
// protocol, after the handlers and the contract document. It is now flags in, client
// call, print: every type here is pkg/warren's, and there is no net/http import.
//
// That direction of dependency is also the test ADR-0008 wanted from this arrangement:
// the CLI covers every §6.1 operation, so a client method no command can use is visible
// immediately, and a command that needed to reach around the client into raw HTTP would
// be a capability the published client fails to offer.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/TSHOGX/warren/pkg/warren"
)

func newSandboxCreateCmd() *cobra.Command {
	var (
		templateID    string
		egressDefault string
		cpuMillis     int64
		memoryByte    int64
		diskByte      int64
		idleTimeout   string
		retention     string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new sandbox",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := warren.CreateRequest{
				TemplateID:  templateID,
				CPUMillis:   cpuMillis,
				MemoryByte:  memoryByte,
				DiskByte:    diskByte,
				IdleTimeout: idleTimeout,
				Retention:   retention,
			}
			// An --egress-default given at all means a network declaration is sent, and
			// `default` is the one field that must be present inside it. Omitting the flag
			// omits the whole object, which the engine normalizes to deny-all — those are
			// two different requests and the flag's presence is what picks between them.
			if egressDefault != "" {
				req.Network = &warren.Network{
					Egress: warren.Egress{Default: &egressDefault},
				}
			}
			sb, err := api.Create(context.Background(), req)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), sb.SandboxID)
			return nil
		},
	}

	cmd.Flags().StringVar(&templateID, "template", "", "template ID (required)")
	cmd.Flags().StringVar(&egressDefault, "egress-default", "", "egress default policy: allow or deny")
	cmd.Flags().Int64Var(&cpuMillis, "cpu-millis", 0, "CPU millicores cap")
	cmd.Flags().Int64Var(&memoryByte, "memory-byte", 0, "memory cap in bytes")
	cmd.Flags().Int64Var(&diskByte, "disk-byte", 0, "disk cap in bytes")
	cmd.Flags().StringVar(&idleTimeout, "idle-timeout", "", "idle timeout duration (e.g. 30m)")
	cmd.Flags().StringVar(&retention, "retention", "", "retention duration (e.g. 24h)")
	_ = cmd.MarkFlagRequired("template")

	return cmd
}

func newSandboxListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List sandboxes",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := api.List(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SANDBOX ID\tSTATE\tDEATH CAUSE")
			for _, sb := range list {
				cause := sb.DeathCause
				if cause == "" {
					cause = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", sb.SandboxID, sb.State, cause)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newSandboxGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <sandbox-id>",
		Short: "Show one sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := api.Get(context.Background(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), sb)
		},
	}
	return cmd
}

func newSandboxPatchCmd() *cobra.Command {
	var (
		cpuMillis   int64
		memoryByte  int64
		diskByte    int64
		idleTimeout string
		retention   string
	)

	cmd := &cobra.Command{
		Use:   "patch <sandbox-id>",
		Short: "Rewrite parts of a sandbox's binding",
		Long: `Rewrite parts of a sandbox's binding.

Only the flags you pass are sent, which is what makes "leave this axis alone"
distinguishable from "set it to 0" — and 0 is itself meaningful here (0 = unlimited for
a cap, retention 0 = never evict).

Resource caps are editable while paused; see the API contract for which axes a running
sandbox refuses.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req warren.PatchRequest
			// Flags().Changed rather than a zero check, for the reason in the Long help:
			// --cpu-millis 0 means "uncap this axis" and must reach the engine, where a
			// zero test would silently drop it.
			if cmd.Flags().Changed("cpu-millis") {
				req.CPUMillis = &cpuMillis
			}
			if cmd.Flags().Changed("memory-byte") {
				req.MemoryByte = &memoryByte
			}
			if cmd.Flags().Changed("disk-byte") {
				req.DiskByte = &diskByte
			}
			if cmd.Flags().Changed("idle-timeout") {
				req.IdleTimeout = &idleTimeout
			}
			if cmd.Flags().Changed("retention") {
				req.Retention = &retention
			}
			sb, err := api.Patch(context.Background(), args[0], req)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), sb)
		},
	}

	cmd.Flags().Int64Var(&cpuMillis, "cpu-millis", 0, "CPU millicores cap (0 = unlimited)")
	cmd.Flags().Int64Var(&memoryByte, "memory-byte", 0, "memory cap in bytes (0 = unlimited)")
	cmd.Flags().Int64Var(&diskByte, "disk-byte", 0, "disk cap in bytes (0 = unlimited)")
	cmd.Flags().StringVar(&idleTimeout, "idle-timeout", "", "idle timeout duration (e.g. 30m)")
	cmd.Flags().StringVar(&retention, "retention", "", "retention duration (0 = never evict)")

	return cmd
}

func newSandboxPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <sandbox-id>",
		Short: "Pause a sandbox",
		Long: `Pause a sandbox.

In-flight engine writes are drained first, then the sandbox is frozen. The reported
state is read back after the verb, so it is the achieved state rather than the requested
one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := api.Pause(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", res.SandboxID, res.State)
			return nil
		},
	}
}

func newSandboxResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <sandbox-id>",
		Short: "Resume a paused sandbox",
		Long: `Resume a paused sandbox.

The sandbox comes back on a FRESH placement, so its host endpoint and any published
port endpoints are new. Re-read them rather than reusing what you held before the
pause.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := api.Resume(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", res.SandboxID, res.State)
			return nil
		},
	}
}

func newSandboxActivityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activity <sandbox-id>",
		Short: "Record one activity now, holding off the idle sweep",
		Long: `Record one activity on a running sandbox.

For work the engine cannot see: a long computation that does no I/O through the
engine's own paths still looks idle to it, and this says otherwise.

It carries NO DURATION. It records one activity now — it is not a lease, not a
keepalive, and not a pause-inhibit, because each of those would have the engine promise
to protect the sandbox for a period. To hold a sandbox open, call this on an interval
shorter than its idle timeout; the interval is yours.

A paused sandbox is a conflict, not a resume: the clock lives in the guest, so there is
nothing to stamp while frozen. An unreachable guest reports unavailable, for the same
reason — there is deliberately no host-side fallback, since a host timestamp would keep
a wedged sandbox alive forever.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := api.RecordActivity(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded activity on %s (idle now %dms)\n", args[0], res.IdleMillis)
			return nil
		},
	}
}

func newSandboxDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <sandbox-id>",
		Short:   "Destroy a sandbox",
		Aliases: []string{"rm"},
		Long: `Destroy a sandbox and its record.

IRREVERSIBLE, and the record goes with it: afterwards a get is a not-found and the
death cause is gone. If you need to know why a sandbox ended, read it before destroying,
or take it from the destroyed event's cause — there is no post-hoc query, because there
is no row left to query.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := api.Destroy(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Destroyed %s\n", args[0])
			return nil
		},
	}
}

// printJSON is the one output helper the commands share. Indented, because a CLI's JSON
// is read by a human at least as often as it is piped.
//
// It takes the WRITER rather than reaching for os.Stdout, so that cobra's SetOut governs
// where output lands. That is what makes a command's output assertable in a test at all —
// os.Stdout would be invisible to it.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
