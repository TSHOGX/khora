// Command warren is the CLI for the sandbox engine's §6.1 API. It is a peer client of
// the same API that a frontend uses — the CLI is a first-class citizen, not a back door.
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

var (
	// Global flags
	apiURL         string
	clientCertFile string
	clientKeyFile  string
	caCertFile     string
	httpClient     = http.DefaultClient
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "warren",
		Short: "Sandbox engine CLI",
		Long: `warren is the CLI for the sandbox engine.

It wraps the §6.1 REST + Connect-RPC API, providing commands for sandbox lifecycle,
file operations, volumes, templates, network configuration, and event streaming.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			client, err := newHTTPClient(clientCertFile, clientKeyFile, caCertFile)
			if err != nil {
				return err
			}
			httpClient = client
			return nil
		},
	}

	rootCmd.PersistentFlags().StringVar(&apiURL, "api", "http://127.0.0.1:8080", "API base URL")
	rootCmd.PersistentFlags().StringVar(&clientCertFile, "client-cert", "", "PEM client certificate for mTLS")
	rootCmd.PersistentFlags().StringVar(&clientKeyFile, "client-key", "", "PEM private key for the mTLS client certificate")
	rootCmd.PersistentFlags().StringVar(&caCertFile, "ca-cert", "", "PEM CA bundle used to verify the API server")

	// Sandbox commands
	sandboxCmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage sandboxes",
		Aliases: []string{"sb"},
	}
	sandboxCmd.AddCommand(
		newSandboxCreateCmd(),
		newSandboxListCmd(),
		newSandboxGetCmd(),
		newSandboxPatchCmd(),
		newSandboxPauseCmd(),
		newSandboxResumeCmd(),
		newSandboxDeleteCmd(),
	)

	// File commands
	filesCmd := &cobra.Command{
		Use:   "files",
		Short: "Upload and download files",
	}
	filesCmd.AddCommand(
		newFilesUploadCmd(),
		newFilesDownloadCmd(),
	)

	// Volume commands
	volumeCmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage volumes",
		Aliases: []string{"vol"},
	}
	volumeCmd.AddCommand(
		newVolumeCreateCmd(),
		newVolumeListCmd(),
		newVolumeGetCmd(),
		newVolumePatchCmd(),
		newVolumeDeleteCmd(),
		newVolumeSnapshotCmd(),
		newVolumeCloneCmd(),
		newVolumeImportCmd(),
		newVolumeExportCmd(),
	)

	// Template commands
	templateCmd := &cobra.Command{
		Use:   "template",
		Short: "Manage templates",
		Aliases: []string{"tpl"},
	}
	templateCmd.AddCommand(
		newTemplateListCmd(),
		newTemplatePromoteCmd(),
		newTemplateDeleteCmd(),
	)

	// Network commands
	networkCmd := &cobra.Command{
		Use:   "network",
		Short: "Manage network configuration",
		Aliases: []string{"net"},
	}
	networkCmd.AddCommand(
		newNetworkGetCmd(),
		newNetworkPatchCmd(),
		newNetworkPublishCmd(),
	)

	// Event commands
	eventsCmd := &cobra.Command{
		Use:   "events",
		Short: "Stream lifecycle events",
	}
	eventsCmd.AddCommand(
		newEventsListCmd(),
		newEventsFollowCmd(),
	)

	// Process commands (Connect-RPC)
	processCmd := &cobra.Command{
		Use:   "process",
		Short: "Manage sandbox processes",
		Aliases: []string{"proc", "ps"},
	}
	processCmd.AddCommand(
		newProcessStartCmd(),
		newProcessListCmd(),
		newProcessSignalCmd(),
		newProcessConnectCmd(),
		newProcessInputCmd(),
	)

	// Health/metrics
	healthCmd := &cobra.Command{
		Use:   "health",
		Short: "Query health and metrics",
	}
	healthCmd.AddCommand(
		newHealthzCmd(),
		newReadyzCmd(),
		newMetricsCmd(),
	)

	rootCmd.AddCommand(
		sandboxCmd,
		filesCmd,
		volumeCmd,
		templateCmd,
		networkCmd,
		eventsCmd,
		processCmd,
		healthCmd,
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
