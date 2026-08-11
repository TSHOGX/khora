package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	transportv1 "warren/internal/transport/gen/engine/transport/v1"
	"warren/internal/transport/gen/engine/transport/v1/transportv1connect"
)

func newProcessStartCmd() *cobra.Command {
	var (
		sandboxID string
		args      []string
		env       []string
		cwd       string
		ptyRows   uint32
		ptyCols   uint32
	)
	cmd := &cobra.Command{
		Use:   "start <command> [args...]",
		Short: "Start a process in a sandbox",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			ctx := cmd.Context()
			baseURL := getBaseURL()

			// Build the transport URL
			transportURL := fmt.Sprintf("http://%s/v1/sandboxes/%s/connect", baseURL, sandboxID)

			client := transportv1connect.NewProcessServiceClient(
				&http.Client{},
				transportURL,
			)

			req := &transportv1.StartRequest{
				Cmd:  cmdArgs[0],
				Args: cmdArgs[1:],
				Env:  env,
				Cwd:  cwd,
			}

			if ptyRows > 0 && ptyCols > 0 {
				req.Pty = &transportv1.PtySize{
					Rows: ptyRows,
					Cols: ptyCols,
				}
			}

			stream, err := client.Start(ctx, connect.NewRequest(req))
			if err != nil {
				return fmt.Errorf("start: %w", err)
			}

			var pid int32
			for stream.Receive() {
				event := stream.Msg()
				switch e := event.Kind.(type) {
				case *transportv1.ProcessEvent_Start:
					pid = e.Start.Pid
					fmt.Fprintf(os.Stderr, "Started process %d\n", pid)
				case *transportv1.ProcessEvent_Data:
					switch e.Data.Stream {
					case "stdout", "pty":
						os.Stdout.Write(e.Data.Data)
					case "stderr":
						os.Stderr.Write(e.Data.Data)
					}
				case *transportv1.ProcessEvent_End:
					if e.End.ExitCode != 0 {
						return fmt.Errorf("process exited with code %d", e.End.ExitCode)
					}
					return nil
				case *transportv1.ProcessEvent_Keepalive:
					// Keepalive received, continue
				}
			}
			if err := stream.Err(); err != nil {
				return fmt.Errorf("stream error: %w", err)
			}

			if pid == 0 {
				return fmt.Errorf("no start event received")
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&sandboxID, "sandbox", "s", "", "Sandbox ID (required)")
	cmd.Flags().StringSliceVarP(&args, "arg", "a", nil, "Additional arguments")
	cmd.Flags().StringSliceVarP(&env, "env", "e", nil, "Environment variables (KEY=VALUE)")
	cmd.Flags().StringVarP(&cwd, "cwd", "d", "", "Working directory")
	cmd.Flags().Uint32Var(&ptyRows, "pty-rows", 0, "PTY rows (requires --pty-cols)")
	cmd.Flags().Uint32Var(&ptyCols, "pty-cols", 0, "PTY columns (requires --pty-rows)")
	cmd.MarkFlagRequired("sandbox")

	return cmd
}

func newProcessListCmd() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List processes in a sandbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			baseURL := getBaseURL()

			transportURL := fmt.Sprintf("http://%s/v1/sandboxes/%s/connect", baseURL, sandboxID)

			client := transportv1connect.NewProcessServiceClient(
				&http.Client{},
				transportURL,
			)

			resp, err := client.List(ctx, connect.NewRequest(&transportv1.ListRequest{}))
			if err != nil {
				return fmt.Errorf("list: %w", err)
			}

			if len(resp.Msg.Pids) == 0 {
				fmt.Println("No processes running")
				return nil
			}

			fmt.Println("PID")
			for _, pid := range resp.Msg.Pids {
				fmt.Println(pid)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&sandboxID, "sandbox", "s", "", "Sandbox ID (required)")
	cmd.MarkFlagRequired("sandbox")

	return cmd
}

func newProcessSignalCmd() *cobra.Command {
	var (
		sandboxID string
		signal    int32
	)
	cmd := &cobra.Command{
		Use:   "signal <pid>",
		Short: "Send a signal to a process",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			pid64, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid pid: %w", err)
			}
			pid := int32(pid64)

			if pid <= 0 {
				return fmt.Errorf("pid must be > 0")
			}

			baseURL := getBaseURL()
			transportURL := fmt.Sprintf("http://%s/v1/sandboxes/%s/connect", baseURL, sandboxID)

			client := transportv1connect.NewProcessServiceClient(
				&http.Client{},
				transportURL,
			)

			req := &transportv1.SendSignalRequest{
				Pid:    pid,
				Signal: signal,
			}

			_, err = client.SendSignal(ctx, connect.NewRequest(req))
			if err != nil {
				return fmt.Errorf("signal: %w", err)
			}

			fmt.Printf("Sent signal %d to process %d\n", signal, pid)
			return nil
		},
	}

	cmd.Flags().StringVarP(&sandboxID, "sandbox", "s", "", "Sandbox ID (required)")
	cmd.Flags().Int32Var(&signal, "signal", 15, "Signal number (default: SIGTERM)")
	cmd.MarkFlagRequired("sandbox")

	return cmd
}

func newProcessConnectCmd() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:   "connect <pid>",
		Short: "Connect to a running process",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			pid64, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid pid: %w", err)
			}
			pid := int32(pid64)

			baseURL := getBaseURL()
			transportURL := fmt.Sprintf("http://%s/v1/sandboxes/%s/connect", baseURL, sandboxID)

			client := transportv1connect.NewProcessServiceClient(
				&http.Client{},
				transportURL,
			)

			req := &transportv1.ConnectRequest{Pid: pid}
			stream, err := client.Connect(ctx, connect.NewRequest(req))
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			for stream.Receive() {
				event := stream.Msg()
				switch e := event.Kind.(type) {
				case *transportv1.ProcessEvent_Start:
					fmt.Fprintf(os.Stderr, "Connected to process %d\n", e.Start.Pid)
				case *transportv1.ProcessEvent_Data:
					switch e.Data.Stream {
					case "stdout", "pty":
						os.Stdout.Write(e.Data.Data)
					case "stderr":
						os.Stderr.Write(e.Data.Data)
					}
				case *transportv1.ProcessEvent_End:
					fmt.Fprintf(os.Stderr, "Process exited with code %d\n", e.End.ExitCode)
					if e.End.ExitCode != 0 {
						return fmt.Errorf("process exited with code %d", e.End.ExitCode)
					}
					return nil
				case *transportv1.ProcessEvent_Keepalive:
					// Keepalive received, continue
				}
			}

			return stream.Err()
		},
	}

	cmd.Flags().StringVarP(&sandboxID, "sandbox", "s", "", "Sandbox ID (required)")
	cmd.MarkFlagRequired("sandbox")

	return cmd
}

func newProcessInputCmd() *cobra.Command {
	var (
		sandboxID string
		ptyIn     bool
	)
	cmd := &cobra.Command{
		Use:   "input <pid>",
		Short: "Send stdin to a process",
		Long:  "Reads from stdin and sends to the process",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			pid64, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid pid: %w", err)
			}
			pid := int32(pid64)

			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}

			baseURL := getBaseURL()
			transportURL := fmt.Sprintf("http://%s/v1/sandboxes/%s/connect", baseURL, sandboxID)

			client := transportv1connect.NewProcessServiceClient(
				&http.Client{},
				transportURL,
			)

			req := &transportv1.SendInputRequest{
				Pid:   pid,
				Data:  data,
				PtyIn: ptyIn,
			}

			_, err = client.SendInput(ctx, connect.NewRequest(req))
			if err != nil {
				return fmt.Errorf("send input: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&sandboxID, "sandbox", "s", "", "Sandbox ID (required)")
	cmd.Flags().BoolVar(&ptyIn, "pty", false, "Send to PTY instead of stdin")
	cmd.MarkFlagRequired("sandbox")

	return cmd
}

// getBaseURL extracts the base URL without the API prefix
func getBaseURL() string {
	base := strings.TrimPrefix(apiURL, "http://")
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimSuffix(base, "/v1")
	base = strings.TrimSuffix(base, "/")
	return base
}
