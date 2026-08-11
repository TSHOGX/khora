package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

func newFilesUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <sandbox-id> <local-path> <remote-path>",
		Short: "Upload a file to a sandbox",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			localPath := args[1]
			remotePath := args[2]

			file, err := os.Open(localPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", localPath, err)
			}
			defer file.Close()

			url := fmt.Sprintf("%s/v1/sandboxes/%s/files?path=%s", apiURL, sandboxID, remotePath)
			resp, err := http.Post(url, "application/octet-stream", file)
			if err != nil {
				return fmt.Errorf("POST %s: %w", url, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var result struct {
				Path         string `json:"path"`
				BytesWritten int64  `json:"bytes_written"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Printf("Uploaded %d bytes to %s\n", result.BytesWritten, result.Path)
			return nil
		},
	}
}

func newFilesDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <sandbox-id> <remote-path> [local-path]",
		Short: "Download a file from a sandbox (prints to stdout if local-path omitted)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			remotePath := args[1]
			var localPath string
			if len(args) == 3 {
				localPath = args[2]
			}

			url := fmt.Sprintf("%s/v1/sandboxes/%s/files?path=%s", apiURL, sandboxID, remotePath)
			resp, err := http.Get(url)
			if err != nil {
				return fmt.Errorf("GET %s: %w", url, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var out io.Writer
			if localPath == "" {
				out = os.Stdout
			} else {
				file, err := os.Create(localPath)
				if err != nil {
					return fmt.Errorf("create %s: %w", localPath, err)
				}
				defer file.Close()
				out = file
			}

			n, err := io.Copy(out, resp.Body)
			if err != nil {
				return fmt.Errorf("copy: %w", err)
			}

			if localPath != "" {
				fmt.Fprintf(os.Stderr, "Downloaded %d bytes to %s\n", n, localPath)
			}
			return nil
		},
	}
}
