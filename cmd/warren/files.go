package main

// The file transfer commands, as a shell over pkg/warren. See sandbox.go's header.
//
// Both directions STREAM — the local file is handed to the client as an io.Reader and
// the download is copied straight out of the response. Nothing here reads a file into
// memory first, which is the same property that makes these two routes REST rather than
// RPC on the engine side: a file must not become one message.

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func newFilesUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <sandbox-id> <local-path> <remote-path>",
		Short: "Upload a file to a running sandbox",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID, localPath, remotePath := args[0], args[1], args[2]

			file, err := os.Open(localPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", localPath, err)
			}
			defer file.Close()

			if err := api.WriteFile(context.Background(), sandboxID, remotePath, file); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Uploaded %s to %s:%s\n", localPath, sandboxID, remotePath)
			return nil
		},
	}
}

func newFilesDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <sandbox-id> <remote-path> <local-path>",
		Short: "Download a file from a running sandbox",
		Long: `Download a file from a running sandbox.

Pass - as the local path to write to stdout.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID, remotePath, localPath := args[0], args[1], args[2]

			rc, err := api.ReadFile(context.Background(), sandboxID, remotePath)
			if err != nil {
				return err
			}
			defer rc.Close()

			out := io.Writer(cmd.OutOrStdout())
			if localPath != "-" {
				f, err := os.Create(localPath)
				if err != nil {
					return fmt.Errorf("create %s: %w", localPath, err)
				}
				defer f.Close()
				out = f
			}
			n, err := io.Copy(out, rc)
			if err != nil {
				return fmt.Errorf("download %s: %w", remotePath, err)
			}
			if localPath != "-" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Downloaded %d bytes to %s\n", n, localPath)
			}
			return nil
		},
	}
}
