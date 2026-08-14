// Command khora is the CLI for the sandbox engine's §6.1 API. It is a peer client of
// the same API that a frontend uses — the CLI is a first-class citizen, not a back door.
package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TSHOGX/khora/pkg/khora"
)

// apiBase returns the configured API origin/prefix without a duplicate /v1.
// Both https://host/prefix and https://host/prefix/v1 are supported forms.
func apiBase() string {
	u, err := url.Parse(apiURL)
	if err != nil {
		return strings.TrimRight(apiURL, "/")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	return strings.TrimRight(u.String(), "/")
}

func apiEndpoint(parts ...string) string {
	return apiBase() + "/" + path.Join(parts...)
}

var (
	// Global flags
	apiURL         string
	clientCertFile string
	clientKeyFile  string
	caCertFile     string
	httpClient     = http.DefaultClient
	// api is the thin client every command goes through (ADR-0008 part 2: the CLI is a
	// shell over pkg/khora). Built once in PersistentPreRunE, after the flags are
	// parsed and before any command runs.
	//
	// This is what keeps the client package honest rather than merely present: the CLI
	// exercises every call, so a client method no command can use is visible
	// immediately — and conversely a command that reaches around it into raw HTTP is a
	// capability the published client does not offer.
	api *khora.Client
)

// version is stamped by the Makefile's -X at build time; "dev" means built by a
// bare `go build`. Per-binary rather than a shared package, matching the other
// three: the gateway "imports nothing from internal/" (ADR-0007).
var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use: "khora",
		// cobra serves --version from this field, and it does so before
		// PersistentPreRunE — so asking the CLI what it is does not first try to
		// build an API client against a --api that may not be listening.
		Version: version,
		Short:   "Sandbox engine CLI",
		Long: `khora is the CLI for the sandbox engine.

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
			// Canonicalize the configured prefix once so every REST command using
			// apiURL + "/v1/..." works whether --api ended at the origin or /v1.
			apiURL = apiBase()
			// The thin client shares the CLI's configured transport rather than building
			// a second one: --client-cert / --ca-cert must mean the same thing on every
			// path out of this process, including the Connect data plane.
			api, err = khora.New(apiURL, khora.WithHTTPClient(client))
			if err != nil {
				return err
			}
			return nil
		},
	}

	// cobra's default template prints "khora version X"; the other three binaries
	// print "<name> X", and an operator collecting all four should not have to
	// special-case one. Set via the method because this cobra keeps the field
	// unexported.
	rootCmd.SetVersionTemplate("khora {{.Version}}\n")

	rootCmd.PersistentFlags().StringVar(&apiURL, "api", "http://127.0.0.1:8080", "API base URL")
	rootCmd.PersistentFlags().StringVar(&clientCertFile, "client-cert", "", "PEM client certificate for mTLS")
	rootCmd.PersistentFlags().StringVar(&clientKeyFile, "client-key", "", "PEM private key for the mTLS client certificate")
	rootCmd.PersistentFlags().StringVar(&caCertFile, "ca-cert", "", "PEM CA bundle used to verify the API server")

	// Sandbox commands
	sandboxCmd := &cobra.Command{
		Use:     "sandbox",
		Short:   "Manage sandboxes",
		Aliases: []string{"sb"},
	}
	sandboxCmd.AddCommand(
		newSandboxCreateCmd(),
		newSandboxListCmd(),
		newSandboxGetCmd(),
		newSandboxPatchCmd(),
		newSandboxPauseCmd(),
		newSandboxResumeCmd(),
		newSandboxActivityCmd(),
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
		Use:     "volume",
		Short:   "Manage volumes",
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
		Use:     "template",
		Short:   "Manage templates",
		Aliases: []string{"tpl"},
	}
	templateCmd.AddCommand(
		newTemplateListCmd(),
		newTemplatePromoteCmd(),
		newTemplateDeleteCmd(),
	)

	// Network commands
	networkCmd := &cobra.Command{
		Use:     "network",
		Short:   "Manage network configuration",
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
		Use:     "process",
		Short:   "Manage sandbox processes",
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
