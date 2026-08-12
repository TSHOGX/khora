package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

func newNetworkGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <sandbox-id>",
		Short: "Get network configuration for a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			resp, err := httpClient.Get(apiURL + "/v1/sandboxes/" + sandboxID + "/network")
			if err != nil {
				return fmt.Errorf("GET /v1/sandboxes/%s/network: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			// Pretty-print the JSON
			var prettyJSON bytes.Buffer
			if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
				return fmt.Errorf("format JSON: %w", err)
			}

			fmt.Println(prettyJSON.String())
			return nil
		},
	}
}

func newNetworkPatchCmd() *cobra.Command {
	var networkJSON string

	cmd := &cobra.Command{
		Use:   "patch <sandbox-id>",
		Short: "Update network configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]

			var networkDecl NetworkDecl
			if err := json.Unmarshal([]byte(networkJSON), &networkDecl); err != nil {
				return fmt.Errorf("parse network JSON: %w", err)
			}

			body, err := json.Marshal(struct {
				Network NetworkDecl `json:"network"`
			}{Network: networkDecl})
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			client := httpClient
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

			fmt.Println("Network configuration updated")
			return nil
		},
	}

	cmd.Flags().StringVar(&networkJSON, "json", "", "network configuration as JSON (required)")
	cmd.MarkFlagRequired("json")
	return cmd
}

func newNetworkPublishCmd() *cobra.Command {
	var (
		guestPort int
		proto     string
	)

	cmd := &cobra.Command{
		Use:   "publish <sandbox-id>",
		Short: "Publish a guest port",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]

			req := struct {
				GuestPort int    `json:"guest_port"`
				Proto     string `json:"proto"`
			}{
				GuestPort: guestPort,
				Proto:     proto,
			}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := httpClient.Post(apiURL+"/v1/sandboxes/"+sandboxID+"/publish", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("POST /v1/sandboxes/%s/publish: %w", sandboxID, err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			var result struct {
				HostEndpoint string `json:"host_endpoint"`
			}
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Published %s port %d -> host endpoint %s\n", proto, guestPort, result.HostEndpoint)
			return nil
		},
	}

	cmd.Flags().IntVar(&guestPort, "guest-port", 0, "guest port to publish (required)")
	cmd.Flags().StringVar(&proto, "proto", "tcp", "protocol: tcp or udp")
	cmd.MarkFlagRequired("guest-port")
	return cmd
}
