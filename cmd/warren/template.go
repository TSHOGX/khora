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

type Template struct {
	TemplateID string `json:"template_id"`
	Parent     string `json:"parent"`
	Builtin    bool   `json:"builtin"`
}

func newTemplateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all templates",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := httpClient.Get(apiURL + "/v1/templates")
			if err != nil {
				return fmt.Errorf("GET /v1/templates: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var templates []Template
			if err := json.Unmarshal(body, &templates); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TEMPLATE_ID\tPARENT\tBUILTIN")
			for _, tpl := range templates {
				fmt.Fprintf(w, "%s\t%s\t%t\n", tpl.TemplateID, tpl.Parent, tpl.Builtin)
			}
			w.Flush()

			return nil
		},
	}
}

func newTemplatePromoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote <sandbox-id>",
		Short: "Promote a sandbox to a template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxID := args[0]
			req := struct {
				FromSandboxID string `json:"from_sandbox_id"`
			}{FromSandboxID: sandboxID}

			body, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := httpClient.Post(apiURL+"/v1/templates", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("POST /v1/templates: %w", err)
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
				TemplateID string `json:"template_id"`
			}
			if err := json.Unmarshal(respBody, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			fmt.Println(result.TemplateID)
			return nil
		},
	}
}

func newTemplateDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <template-id>",
		Short:   "Delete a template",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			templateID := args[0]
			client := httpClient
			req, err := http.NewRequest(http.MethodDelete, apiURL+"/v1/templates/"+templateID, nil)
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("DELETE /v1/templates/%s: %w", templateID, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			fmt.Printf("Deleted %s\n", templateID)
			return nil
		},
	}
}
