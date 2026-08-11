package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type CreateVolumeRequest struct {
	Size *int64 `json:"size,omitempty"`
}

type CreateVolumeResponse struct {
	VolumeID string `json:"volume_id"`
	Size     int64  `json:"size"`
}

type Volume struct {
	VolumeID string  `json:"volume_id"`
	Size     int64   `json:"size"`
	Backend  *string `json:"backend,omitempty"`
	UsedByte *int64  `json:"used_byte,omitempty"`
}

type PatchVolumeRequest struct {
	Size int64 `json:"size"`
}

type PatchVolumeResponse struct {
	VolumeID string `json:"volume_id"`
	Size     int64  `json:"size"`
}

func newVolumeCreateCmd() *cobra.Command {
	var size int64

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new volume",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := CreateVolumeRequest{}
			if size > 0 {
				req.Size = &size
			}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := http.Post(apiURL+"/v1/volumes", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("POST /v1/volumes: %w", err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result CreateVolumeResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Println(result.VolumeID)
			return nil
		},
	}

	cmd.Flags().Int64Var(&size, "size", 0, "volume size in bytes")
	return cmd
}

func newVolumeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all volumes",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(apiURL + "/v1/volumes")
			if err != nil {
				return fmt.Errorf("GET /v1/volumes: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var volumes []Volume
			if err := json.Unmarshal(body, &volumes); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "VOLUME_ID\tSIZE\tBACKEND")
			for _, vol := range volumes {
				backend := "-"
				if vol.Backend != nil {
					backend = *vol.Backend
				}
				fmt.Fprintf(w, "%s\t%d\t%s\n", vol.VolumeID, vol.Size, backend)
			}
			w.Flush()

			return nil
		},
	}
}

func newVolumeGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <volume-id>",
		Short: "Get volume details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			resp, err := http.Get(apiURL + "/v1/volumes/" + volumeID)
			if err != nil {
				return fmt.Errorf("GET /v1/volumes/%s: %w", volumeID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var vol Volume
			if err := json.Unmarshal(body, &vol); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Printf("volume_id: %s\n", vol.VolumeID)
			fmt.Printf("size:      %d\n", vol.Size)
			if vol.Backend != nil {
				fmt.Printf("backend:   %s\n", *vol.Backend)
			}
			if vol.UsedByte != nil {
				fmt.Printf("used_byte: %d\n", *vol.UsedByte)
			}

			return nil
		},
	}
}

func newVolumePatchCmd() *cobra.Command {
	var size int64

	cmd := &cobra.Command{
		Use:   "patch <volume-id>",
		Short: "Update volume size",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			req := PatchVolumeRequest{Size: size}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			client := &http.Client{}
			httpReq, err := http.NewRequest(http.MethodPatch, apiURL+"/v1/volumes/"+volumeID, bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(httpReq)
			if err != nil {
				return fmt.Errorf("PATCH /v1/volumes/%s: %w", volumeID, err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result PatchVolumeResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Printf("volume_id: %s\n", result.VolumeID)
			fmt.Printf("size:      %d\n", result.Size)

			return nil
		},
	}

	cmd.Flags().Int64Var(&size, "size", 0, "new volume size in bytes (required)")
	cmd.MarkFlagRequired("size")
	return cmd
}

func newVolumeDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <volume-id>",
		Short:   "Delete a volume",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			client := &http.Client{}
			req, err := http.NewRequest(http.MethodDelete, apiURL+"/v1/volumes/"+volumeID, nil)
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("DELETE /v1/volumes/%s: %w", volumeID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			fmt.Printf("Deleted %s\n", volumeID)
			return nil
		},
	}
}

func newVolumeSnapshotCmd() *cobra.Command {
	var size int64

	cmd := &cobra.Command{
		Use:   "snapshot <volume-id>",
		Short: "Create a snapshot of a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			req := CreateVolumeRequest{}
			if size > 0 {
				req.Size = &size
			}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := http.Post(apiURL+"/v1/volumes/"+volumeID+"/snapshot", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("POST /v1/volumes/%s/snapshot: %w", volumeID, err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result CreateVolumeResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Println(result.VolumeID)
			return nil
		},
	}

	cmd.Flags().Int64Var(&size, "size", 0, "snapshot size in bytes")
	return cmd
}

func newVolumeCloneCmd() *cobra.Command {
	var size int64

	cmd := &cobra.Command{
		Use:   "clone <volume-id>",
		Short: "Create a clone of a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			req := CreateVolumeRequest{}
			if size > 0 {
				req.Size = &size
			}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := http.Post(apiURL+"/v1/volumes/"+volumeID+"/clone", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("POST /v1/volumes/%s/clone: %w", volumeID, err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result CreateVolumeResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Println(result.VolumeID)
			return nil
		},
	}

	cmd.Flags().Int64Var(&size, "size", 0, "clone size in bytes")
	return cmd
}

func newVolumeImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <volume-id> <local-path> <remote-path>",
		Short: "Import a file into a volume",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			localPath := args[1]
			remotePath := args[2]

			file, err := os.Open(localPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", localPath, err)
			}
			defer file.Close()

			url := fmt.Sprintf("%s/v1/volumes/%s/import?path=%s", apiURL, volumeID, remotePath)
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
				VolumeID     string `json:"volume_id"`
				Path         string `json:"path"`
				BytesWritten int64  `json:"bytes_written"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Printf("Imported %d bytes to %s\n", result.BytesWritten, result.Path)
			return nil
		},
	}
}

func newVolumeExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <volume-id> <remote-path> [local-path]",
		Short: "Export a file from a volume (prints to stdout if local-path omitted)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeID := args[0]
			remotePath := args[1]
			var localPath string
			if len(args) == 3 {
				localPath = args[2]
			}

			url := fmt.Sprintf("%s/v1/volumes/%s/export?path=%s", apiURL, volumeID, remotePath)
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
				fmt.Fprintf(os.Stderr, "Exported %d bytes to %s\n", n, localPath)
			}
			return nil
		},
	}
}
