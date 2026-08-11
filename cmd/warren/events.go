package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type Event struct {
	SequenceNum int64     `json:"sequence_num"`
	Timestamp   time.Time `json:"timestamp"`
	SandboxID   string    `json:"sandbox_id"`
	Event       string    `json:"event"`
	State       string    `json:"state"`
}

func newEventsListCmd() *cobra.Command {
	var (
		after  int64
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List lifecycle events",
		RunE: func(cmd *cobra.Command, args []string) error {
			url := apiURL + "/v1/events"
			params := []string{}
			if after > 0 {
				params = append(params, fmt.Sprintf("after=%d", after))
			}
			if limit > 0 {
				params = append(params, fmt.Sprintf("limit=%d", limit))
			}
			if cursor != "" {
				params = append(params, fmt.Sprintf("cursor=%s", cursor))
			}
			if len(params) > 0 {
				url += "?" + strings.Join(params, "&")
			}

			resp, err := http.Get(url)
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

			var result struct {
				Events []Event `json:"events"`
				Cursor *string `json:"cursor,omitempty"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SEQ\tTIMESTAMP\tSANDBOX_ID\tEVENT\tSTATE")
			for _, evt := range result.Events {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
					evt.SequenceNum,
					evt.Timestamp.Format(time.RFC3339),
					evt.SandboxID,
					evt.Event,
					evt.State)
			}
			w.Flush()

			if result.Cursor != nil {
				fmt.Fprintf(os.Stderr, "\nNext cursor: %s\n", *result.Cursor)
			}

			return nil
		},
	}

	cmd.Flags().Int64Var(&after, "after", 0, "sequence number to start after")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum events to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "pagination cursor from previous response")
	return cmd
}

func newEventsFollowCmd() *cobra.Command {
	var after int64

	cmd := &cobra.Command{
		Use:     "follow",
		Short:   "Follow lifecycle events in real-time",
		Aliases: []string{"tail", "watch"},
		RunE: func(cmd *cobra.Command, args []string) error {
			url := apiURL + "/v1/events/stream"
			if after > 0 {
				url += fmt.Sprintf("?after=%d", after)
			}

			resp, err := http.Get(url)
			if err != nil {
				return fmt.Errorf("GET /v1/events/stream: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			}

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}

				// SSE format: "data: {...}"
				if strings.HasPrefix(line, "data: ") {
					jsonData := strings.TrimPrefix(line, "data: ")
					var evt Event
					if err := json.Unmarshal([]byte(jsonData), &evt); err != nil {
						fmt.Fprintf(os.Stderr, "Failed to parse event: %v\n", err)
						continue
					}

					fmt.Printf("%s [%s] %s %s\n",
						evt.Timestamp.Format("15:04:05"),
						evt.SandboxID,
						evt.Event,
						evt.State)
				}
			}

			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read stream: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().Int64Var(&after, "after", 0, "sequence number to start after")
	return cmd
}
