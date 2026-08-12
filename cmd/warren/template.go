package main

// The template commands, as a shell over pkg/warren. See sandbox.go's header.

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newTemplateListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List templates",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := api.Templates(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TEMPLATE ID\tBUILTIN\tPROMOTED FROM")
			for _, t := range list {
				parent := t.Parent
				if parent == "" {
					parent = "-"
				}
				fmt.Fprintf(w, "%s\t%t\t%s\n", t.TemplateID, t.Builtin, parent)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newTemplatePromoteCmd() *cobra.Command {
	var fromSandbox string

	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Promote a sandbox's filesystem into a reusable template",
		Long: `Promote a sandbox's filesystem into a reusable template.

The template ID is minted by the engine and printed on success — it is not yours to
choose, because the engine's IDs are the only names it guarantees.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := api.Promote(context.Background(), fromSandbox)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}

	cmd.Flags().StringVar(&fromSandbox, "from", "", "the sandbox to promote (required)")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

func newTemplateDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <template-id>",
		Short:   "Delete a template",
		Aliases: []string{"rm"},
		Long: `Delete a template.

A template some sandbox was built from is refused as a conflict: its layers are still in
use underneath those sandboxes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := api.DeleteTemplate(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", args[0])
			return nil
		},
	}
}
