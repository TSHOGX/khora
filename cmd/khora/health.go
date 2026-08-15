package main

// The operational probes: /healthz, /readyz, /metrics.
//
// THESE ARE NOT the control-plane API OPERATIONS, and that is why this file is the one place the CLI
// still speaks raw HTTP rather than going through pkg/khora. The three are excluded
// from the contract document deliberately (openapi.go says so): they are the surface a
// supervisor, a load balancer, and a scraper use, addressed to the PROCESS rather than
// to the sandbox model.
//
// One exception: `readyz` does go through the client, "should I send traffic" being a question an
// API CONSUMER acts on — the check before the first call. `healthz` ("is the process alive") and
// `metrics` (a Prometheus scrape) address an audience outside the API entirely, so putting them
// on a control-plane API client would widen it past what it claims to be.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/TSHOGX/khora/pkg/khora"
)

// probe issues a bare GET and prints status plus body. Used only by the two endpoints
// that are outside the API surface — see the file header.
func probe(cmd *cobra.Command, route string) error {
	resp, err := httpClient.Get(apiURL + route)
	if err != nil {
		return fmt.Errorf("GET %s: %w", route, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "HTTP %d\n", resp.StatusCode)
	fmt.Fprintln(cmd.OutOrStdout(), string(body))
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	return nil
}

func newHealthzCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "healthz",
		Short: "Check that the engine process is alive",
		Long: `Check that the engine process is alive.

Liveness only: it answers as long as the process is serving, whether or not it can
actually place a sandbox. Use readyz for that.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return probe(cmd, "/healthz")
		},
	}
}

func newReadyzCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "readyz",
		Short: "Check that the engine is ready to serve",
		RunE: func(cmd *cobra.Command, args []string) error {
			err := api.Ready(context.Background())
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "ready")
				return nil
			}
			// A non-2xx is the endpoint working correctly and reporting not-ready, which is
			// information rather than a CLI failure — so it is printed and exits non-zero,
			// where a transport error is returned as an ordinary error.
			var apiErr *khora.Error
			if errors.As(err, &apiErr) {
				fmt.Fprintf(cmd.OutOrStdout(), "not ready: HTTP %d\n%s\n", apiErr.Code, apiErr.Message)
				os.Exit(1)
			}
			return err
		},
	}
}

func newMetricsCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Dump the Prometheus metrics exposition",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := httpClient.Get(apiURL + "/metrics")
			if err != nil {
				return fmt.Errorf("GET /metrics: %w", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				fmt.Fprintf(cmd.OutOrStdout(), "HTTP %d\n%s\n", resp.StatusCode, body)
				os.Exit(1)
			}

			out := cmd.OutOrStdout()
			switch format {
			case "raw":
				fmt.Fprintln(out, string(body))
			case "json":
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(parsePrometheusMetrics(body)); err != nil {
					return fmt.Errorf("encode metrics as JSON: %w", err)
				}
			case "table":
				metrics := parsePrometheusMetrics(body)
				names := make([]string, 0, len(metrics))
				for name := range metrics {
					names = append(names, name)
				}
				// Sorted, so two runs are diffable — map order would make every dump look
				// different from the last for no reason.
				sort.Strings(names)
				w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "METRIC\tVALUE")
				for _, name := range names {
					fmt.Fprintf(w, "%s\t%s\n", name, metrics[name])
				}
				return w.Flush()
			default:
				return fmt.Errorf("unsupported format %q (expected raw, table, or json)", format)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "raw", "output format: raw, table, or json")
	return cmd
}

// parsePrometheusMetrics pulls `name value` pairs out of an exposition body, skipping
// comments. Enough for the CLI's table and json views — a caller that needs labels
// parsed properly should scrape the raw output with a real client.
func parsePrometheusMetrics(body []byte) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		result[name] = value
	}
	return result
}
