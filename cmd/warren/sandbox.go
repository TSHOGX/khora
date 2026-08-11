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

// Sandbox API types matching §6.1
type CreateSandboxRequest struct {
	TemplateID    string            `json:"template_id"`
	Network       *NetworkDecl      `json:"network,omitempty"`
	Volumes       []VolumeMount     `json:"volumes,omitempty"`
	IdleTimeout   *string           `json:"idle_timeout,omitempty"`
	Retention     *string           `json:"retention,omitempty"`
	CPUMillis     *int64            `json:"cpu_millis,omitempty"`
	MemoryByte    *int64            `json:"memory_byte,omitempty"`
	DiskByte      *int64            `json:"disk_byte,omitempty"`
}

type NetworkDecl struct {
	Egress  EgressDecl  `json:"egress"`
	Ingress IngressDecl `json:"ingress"`
}

type EgressDecl struct {
	Default string        `json:"default"`
	Allow   []AllowRule   `json:"allow"`
	Via     []ViaRule     `json:"via"`
}

type IngressDecl struct {
	Publish []PublishRule `json:"publish"`
}

type AllowRule struct {
	Target string `json:"target"`
}

type ViaRule struct {
	Endpoint string      `json:"endpoint"`
	Match    []AllowRule `json:"match"`
}

type PublishRule struct {
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

type VolumeMount struct {
	VolumeID string `json:"volume_id"`
	Path     string `json:"path"`
	Access   string `json:"access"`
}

type CreateSandboxResponse struct {
	SandboxID string `json:"sandbox_id"`
}

type Sandbox struct {
	SandboxID  string  `json:"sandbox_id"`
	State      string  `json:"state"`
	DeathCause *string `json:"death_cause,omitempty"`
	UsedByte   *int64  `json:"used_byte,omitempty"`
}

type PatchSandboxRequest struct {
	CPUMillis   *int64         `json:"cpu_millis,omitempty"`
	MemoryByte  *int64         `json:"memory_byte,omitempty"`
	DiskByte    *int64         `json:"disk_byte,omitempty"`
	IdleTimeout *string        `json:"idle_timeout,omitempty"`
	Retention   *string        `json:"retention,omitempty"`
	Volumes     *[]VolumeMount `json:"volumes,omitempty"`
	Network     *NetworkDecl   `json:"network,omitempty"`
}

type PatchSandboxResponse struct {
	SandboxID      string `json:"sandbox_id"`
	State          string `json:"state"`
	AppliesAt      string `json:"applies_at"`
	LeaseReleased  *bool  `json:"lease_released,omitempty"`
}

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
			req := CreateSandboxRequest{
				TemplateID: templateID,
			}

			if egressDefault != "" {
				req.Network = &NetworkDecl{
					Egress: EgressDecl{
						Default: egressDefault,
						Allow:   []AllowRule{},
						Via:     []ViaRule{},
					},
					Ingress: IngressDecl{
						Publish: []PublishRule{},
					},
				}
			}

			if cpuMillis > 0 {
				req.CPUMillis = &cpuMillis
			}
			if memoryByte > 0 {
				req.MemoryByte = &memoryByte
			}
			if diskByte > 0 {
				req.DiskByte = &diskByte
			}
			if idleTimeout != "" {
				req.IdleTimeout = &idleTimeout
			}
			if retention != "" {
				req.Retention = &retention
			}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := http.Post(apiURL+"/v1/sandboxes", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("POST /v1/sandboxes: %w", err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result CreateSandboxResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Println(result.SandboxID)
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
	cmd.MarkFlagRequired("template")

	return cmd
}

func newSandboxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all sandboxes",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(apiURL + "/v1/sandboxes")
			if err != nil {
				return fmt.Errorf("GET /v1/sandboxes: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var sandboxes []Sandbox
			if err := json.Unmarshal(body, &sandboxes); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SANDBOX_ID\tSTATE\tDEATH_CAUSE\tUSED_BYTE")
			for _, sb := range sandboxes {
				deathCause := "-"
				if sb.DeathCause != nil {
					deathCause = *sb.DeathCause
				}
				usedByte := "-"
				if sb.UsedByte != nil {
					usedByte = fmt.Sprintf("%d", *sb.UsedByte)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sb.SandboxID, sb.State, deathCause, usedByte)
			}
			w.Flush()

			return nil
		},
	}
}

func newSandboxGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <sandbox-id>",
		Short: "Get sandbox details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			resp, err := http.Get(apiURL + "/v1/sandboxes/" + sandboxID)
			if err != nil {
				return fmt.Errorf("GET /v1/sandboxes/%s: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var sb Sandbox
			if err := json.Unmarshal(body, &sb); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Printf("sandbox_id:  %s\n", sb.SandboxID)
			fmt.Printf("state:       %s\n", sb.State)
			if sb.DeathCause != nil {
				fmt.Printf("death_cause: %s\n", *sb.DeathCause)
			}
			if sb.UsedByte != nil {
				fmt.Printf("used_byte:   %d\n", *sb.UsedByte)
			}

			return nil
		},
	}
}

func newSandboxPatchCmd() *cobra.Command {
	var (
		cpuMillis   *int64
		memoryByte  *int64
		diskByte    *int64
		idleTimeout *string
		retention   *string
	)

	cmd := &cobra.Command{
		Use:   "patch <sandbox-id>",
		Short: "Update sandbox binding",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			req := PatchSandboxRequest{
				CPUMillis:   cpuMillis,
				MemoryByte:  memoryByte,
				DiskByte:    diskByte,
				IdleTimeout: idleTimeout,
				Retention:   retention,
			}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			client := &http.Client{}
			httpReq, err := http.NewRequest(http.MethodPatch, apiURL+"/v1/sandboxes/"+sandboxID, bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(httpReq)
			if err != nil {
				return fmt.Errorf("PATCH /v1/sandboxes/%s: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result PatchSandboxResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Printf("sandbox_id:  %s\n", result.SandboxID)
			fmt.Printf("state:       %s\n", result.State)
			fmt.Printf("applies_at:  %s\n", result.AppliesAt)
			if result.LeaseReleased != nil && *result.LeaseReleased {
				fmt.Println("lease_released: true")
			}

			return nil
		},
	}

	var cpuMillisVal int64
	var memoryByteVal int64
	var diskByteVal int64
	var idleTimeoutVal string
	var retentionVal string

	cmd.Flags().Int64Var(&cpuMillisVal, "cpu-millis", 0, "CPU millicores cap")
	cmd.Flags().Int64Var(&memoryByteVal, "memory-byte", 0, "memory cap in bytes")
	cmd.Flags().Int64Var(&diskByteVal, "disk-byte", 0, "disk cap in bytes")
	cmd.Flags().StringVar(&idleTimeoutVal, "idle-timeout", "", "idle timeout duration")
	cmd.Flags().StringVar(&retentionVal, "retention", "", "retention duration")

	// Bind flag values to the pointers used in the request
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("cpu-millis") {
			cpuMillis = &cpuMillisVal
		}
		if cmd.Flags().Changed("memory-byte") {
			memoryByte = &memoryByteVal
		}
		if cmd.Flags().Changed("disk-byte") {
			diskByte = &diskByteVal
		}
		if cmd.Flags().Changed("idle-timeout") {
			idleTimeout = &idleTimeoutVal
		}
		if cmd.Flags().Changed("retention") {
			retention = &retentionVal
		}
		return nil
	}

	return cmd
}

func newSandboxPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <sandbox-id>",
		Short: "Pause a running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			resp, err := http.Post(apiURL+"/v1/sandboxes/"+sandboxID+"/pause", "application/json", nil)
			if err != nil {
				return fmt.Errorf("POST /v1/sandboxes/%s/pause: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			fmt.Printf("Paused %s\n", sandboxID)
			return nil
		},
	}
}

func newSandboxResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <sandbox-id>",
		Short: "Resume a paused sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			resp, err := http.Post(apiURL+"/v1/sandboxes/"+sandboxID+"/resume", "application/json", nil)
			if err != nil {
				return fmt.Errorf("POST /v1/sandboxes/%s/resume: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			fmt.Printf("Resumed %s\n", sandboxID)
			return nil
		},
	}
}

func newSandboxDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <sandbox-id>",
		Short:   "Delete a sandbox",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			client := &http.Client{}
			req, err := http.NewRequest(http.MethodDelete, apiURL+"/v1/sandboxes/"+sandboxID, nil)
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("DELETE /v1/sandboxes/%s: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			fmt.Printf("Deleted %s\n", sandboxID)
			return nil
		},
	}
}
