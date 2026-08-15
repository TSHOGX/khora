package main

// The event commands, as a shell over pkg/khora. See sandbox.go's header.
//
// `follow` is the one command that holds a LOOP, and the loop stays here rather than
// moving into the client on purpose: cursor advance, page draining, and the poll interval
// are policy — promises about time — and the thin-client contract keeps those on the caller's side of the
// boundary. What the client contributes is the long poll, which is the engine's own
// `wait=` parameter, so the waiting happens where the events are.

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/TSHOGX/khora/pkg/khora"
)

func newEventsListCmd() *cobra.Command {
	var (
		after int64
		limit int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List lifecycle events",
		RunE: func(cmd *cobra.Command, args []string) error {
			page, err := api.Events(cmd.Context(), khora.EventsOptions{After: after, Limit: limit})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SEQ\tTIMESTAMP\tSANDBOX_ID\tEVENT\tSTATE\tCAUSE")
			for _, e := range page.Events {
				cause := e.Cause
				if cause == "" {
					cause = "-"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
					e.Seq, e.OccurredAt, e.SandboxID, e.Kind, e.State, cause)
			}
			return w.Flush()
		},
	}

	cmd.Flags().Int64Var(&after, "after", 0, "sequence number to start after")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum events to return")
	return cmd
}

func newEventsFollowCmd() *cobra.Command {
	var (
		after    int64
		interval time.Duration
		wait     time.Duration
	)

	cmd := &cobra.Command{
		Use:     "follow",
		Short:   "Follow lifecycle events by polling cursor replay",
		Aliases: []string{"tail", "watch"},
		Long: `Follow lifecycle events.

Replay is by cursor, and delivery is at-least-once: an event can be reported twice, so
anything consuming this programmatically should dedup on the event ID rather than trust
the sequence alone.

--wait hands the waiting to the engine, which returns the moment an event lands. That is
why --interval is not the detection latency: it is only reached after a wait expires with
nothing new.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			for {
				page, err := api.Events(cmd.Context(), khora.EventsOptions{
					After: after,
					Limit: 1000,
					Wait:  wait,
				})
				if err != nil {
					// A cancelled context is how this command ends normally — Ctrl-C — so it
					// is not an error to report.
					if cmd.Context().Err() != nil {
						return nil
					}
					return err
				}
				for _, e := range page.Events {
					cause := ""
					if e.Cause != "" {
						cause = " (" + e.Cause + ")"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s [%s] %s %s%s\n",
						e.OccurredAt, e.SandboxID, e.Kind, e.State, cause)
					if e.Seq > after {
						after = e.Seq
					}
				}
				if len(page.Events) > 0 {
					continue // drain the next page without adding polling latency
				}

				timer := time.NewTimer(interval)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		},
	}

	cmd.Flags().Int64Var(&after, "after", 0, "sequence number to start after")
	cmd.Flags().DurationVar(&interval, "interval", time.Second,
		"poll interval when caught up (only reached after --wait expires with nothing new)")
	cmd.Flags().DurationVar(&wait, "wait", 20*time.Second,
		"hold each request open this long when caught up, so an event is reported as it lands "+
			"rather than at the next --interval tick. The engine clamps its own ceiling; 0 = pure polling")
	return cmd
}
