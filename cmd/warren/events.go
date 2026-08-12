package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type Event struct {
	Seq        int64     `json:"seq"`
	OccurredAt time.Time `json:"occurred_at"`
	SandboxID  string    `json:"sandbox_id"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
}

type eventsResponse struct {
	Events   []Event `json:"events"`
	Earliest int64   `json:"earliest"`
	Latest   int64   `json:"latest"`
}

func newEventsListCmd() *cobra.Command {
	var (
		after int64
		limit int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List lifecycle events",
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, err := eventsURL()
			if err != nil {
				return err
			}
			params := endpoint.Query()
			if after > 0 {
				params.Set("after", strconv.FormatInt(after, 10))
			}
			if limit > 0 {
				params.Set("limit", strconv.Itoa(limit))
			}
			endpoint.RawQuery = params.Encode()

			resp, err := http.Get(endpoint.String())
			if err != nil {
				return fmt.Errorf("GET /v1/events: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			var result eventsResponse
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SEQ\tTIMESTAMP\tSANDBOX_ID\tEVENT\tSTATE")
			for _, evt := range result.Events {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
					evt.Seq,
					evt.OccurredAt.Format(time.RFC3339),
					evt.SandboxID,
					evt.Kind,
					evt.State)
			}
			w.Flush()

			return nil
		},
	}

	cmd.Flags().Int64Var(&after, "after", 0, "sequence number to start after")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum events to return")
	return cmd
}

func newEventsFollowCmd() *cobra.Command {
	var (
		after    int64
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:     "follow",
		Short:   "Follow lifecycle events by polling cursor replay",
		Aliases: []string{"tail", "watch"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := &http.Client{}
			for {
				endpoint, err := eventsURL()
				if err != nil {
					return err
				}
				q := endpoint.Query()
				q.Set("after", strconv.FormatInt(after, 10))
				q.Set("limit", "1000")
				endpoint.RawQuery = q.Encode()

				req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint.String(), nil)
				if err != nil {
					return fmt.Errorf("build GET /v1/events: %w", err)
				}
				resp, err := client.Do(req)
				if err != nil {
					if cmd.Context().Err() != nil {
						return nil
					}
					return fmt.Errorf("GET /v1/events: %w", err)
				}
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr != nil {
					return fmt.Errorf("read response: %w", readErr)
				}
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
				}

				var result eventsResponse
				if err := json.Unmarshal(body, &result); err != nil {
					return fmt.Errorf("unmarshal response: %w", err)
				}
				for _, evt := range result.Events {
					fmt.Printf("%s [%s] %s %s\n", evt.OccurredAt.Format("15:04:05"), evt.SandboxID, evt.Kind, evt.State)
					if evt.Seq > after {
						after = evt.Seq
					}
				}
				if len(result.Events) > 0 {
					continue // drain the next page without adding polling latency
				}

				timer := time.NewTimer(interval)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		},
	}

	cmd.Flags().Int64Var(&after, "after", 0, "sequence number to start after")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "poll interval when caught up")
	return cmd
}

func eventsURL() (*url.URL, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid --api URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid --api URL: scheme must be http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid --api URL: host is required")
	}
	basePath := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.Path = basePath + "/v1/events"
	u.RawPath = ""
	u.Fragment = ""
	return u, nil
}
