package main

// The volume commands, as a shell over pkg/khora. See sandbox.go's header.

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newVolumeCreateCmd() *cobra.Command {
	var size int64
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a volume",
		Long: `Create a volume.

A volume's lifetime is independent of any sandbox's — that is the point of the
subsystem. Attach it by naming it in a sandbox's volume binding at create time or on a
patch.

--size 0 means uncapped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := api.CreateVolume(context.Background(), size)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), v.VolumeID)
			return nil
		},
	}
	cmd.Flags().Int64Var(&size, "size", 0, "volume size in bytes (0 = uncapped)")
	return cmd
}

func newVolumeListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List volumes",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := api.Volumes(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "VOLUME ID\tSIZE\tBACKEND")
			for _, v := range list {
				backend := v.Backend
				if backend == "" {
					backend = "-"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\n", v.VolumeID, v.Size, backend)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newVolumeGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <volume-id>",
		Short: "Show one volume, including its usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := api.Volume(context.Background(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), v)
		},
	}
}

func newVolumePatchCmd() *cobra.Command {
	var size int64
	cmd := &cobra.Command{
		Use:   "patch <volume-id>",
		Short: "Resize a volume",
		Long: `Resize a volume.

A cap that is not strictly greater than current usage is refused rather than applied —
shrinking below what is stored would make the quota unenforceable rather than free any
space. Run 'volume get' first if you need to know the floor.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := api.ResizeVolume(context.Background(), args[0], size)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), v)
		},
	}
	cmd.Flags().Int64Var(&size, "size", 0, "new volume size in bytes (required)")
	_ = cmd.MarkFlagRequired("size")
	return cmd
}

func newVolumeDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <volume-id>",
		Short:   "Delete a volume",
		Aliases: []string{"rm"},
		Long: `Delete a volume.

A volume some sandbox still binds is refused as a conflict — detach it first by patching
that sandbox's volume binding.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := api.DeleteVolume(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", args[0])
			return nil
		},
	}
}

func newVolumeSnapshotCmd() *cobra.Command {
	var size int64
	cmd := &cobra.Command{
		Use:   "snapshot <volume-id>",
		Short: "Derive a new volume from this one's current content",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := api.SnapshotVolume(context.Background(), args[0], size)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().Int64Var(&size, "size", 0, "snapshot size in bytes (0 = uncapped)")
	return cmd
}

func newVolumeCloneCmd() *cobra.Command {
	var size int64
	cmd := &cobra.Command{
		Use:   "clone <volume-id>",
		Short: "Derive a new independent volume from this one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := api.CloneVolume(context.Background(), args[0], size)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().Int64Var(&size, "size", 0, "clone size in bytes (0 = uncapped)")
	return cmd
}

func newVolumeImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <volume-id> <local-path> <remote-path>",
		Short: "Write a file into a volume, with no sandbox involved",
		Long: `Write a file into a volume directly.

No running sandbox is needed: the host reaches the volume through a root-confined
accessor, so a volume can be seeded before anything mounts it. The remote path is
relative to the volume root, and a traversal clamps inside the volume rather than
erroring.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID, localPath, remotePath := args[0], args[1], args[2]

			file, err := os.Open(localPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", localPath, err)
			}
			defer file.Close()

			res, err := api.ImportVolume(context.Background(), volumeID, remotePath, file)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %d bytes to %s:%s\n",
				res.BytesWritten, volumeID, res.Path)
			return nil
		},
	}
}

func newVolumeExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <volume-id> <remote-path> [local-path]",
		Short: "Read a file from a volume, with no sandbox involved (stdout if local-path omitted)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID, remotePath := args[0], args[1]
			localPath := ""
			if len(args) == 3 {
				localPath = args[2]
			}

			rc, err := api.ExportVolume(context.Background(), volumeID, remotePath)
			if err != nil {
				return err
			}
			defer rc.Close()

			out := io.Writer(cmd.OutOrStdout())
			if localPath != "" && localPath != "-" {
				f, err := os.Create(localPath)
				if err != nil {
					return fmt.Errorf("create %s: %w", localPath, err)
				}
				defer f.Close()
				out = f
			}
			n, err := io.Copy(out, rc)
			if err != nil {
				return fmt.Errorf("export %s: %w", remotePath, err)
			}
			if localPath != "" && localPath != "-" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Exported %d bytes to %s\n", n, localPath)
			}
			return nil
		},
	}
}
