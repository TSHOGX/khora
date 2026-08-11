package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newHealthzCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "healthz",
		Short: "Check liveness",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(apiURL + "/healthz")
			if err != nil {
				return fmt.Errorf("GET /healthz: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			fmt.Printf("HTTP %d\n", resp.StatusCode)
			fmt.Println(string(body))

			if resp.StatusCode != http.StatusOK {
				os.Exit(1)
			}

			return nil
		},
	}
}

func newReadyzCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "readyz",
		Short: "Check readiness",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(apiURL + "/readyz")
			if err != nil {
				return fmt.Errorf("GET /readyz: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			fmt.Printf("HTTP %d\n", resp.StatusCode)
			fmt.Println(string(body))

			if resp.StatusCode != http.StatusOK {
				os.Exit(1)
			}

			return nil
		},
	}
}

func newMetricsCmd() *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Fetch process metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(apiURL + "/metrics")
			if err != nil {
				return fmt.Errorf("GET /metrics: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			if format == "json" {
				// Parse Prometheus text format and convert to JSON (basic)
				fmt.Println(string(body))
			} else if format == "table" {
				// Parse and format as table
				lines := parsePrometheusMetrics(body)
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "METRIC\tVALUE")
				for metric, value := range lines {
					fmt.Fprintf(w, "%s\t%s\n", metric, value)
				}
				w.Flush()
			} else {
				// Raw prometheus format
				fmt.Println(string(body))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "raw", "output format: raw, table, or json")
	return cmd
}

func parsePrometheusMetrics(body []byte) map[string]string {
	// Simple parser for demonstration
	result := make(map[string]string)
	lines := string(body)
	for _, line := range splitLines(lines) {
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		// Very basic: split on first space
		parts := splitOnce(line, " ")
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

func splitLines(s string) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}

func splitOnce(s, sep string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep[0] {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}
