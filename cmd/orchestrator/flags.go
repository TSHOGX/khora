package main

import (
	"flag"
	"time"
)

type orchestratorFlags struct {
	showVersion         bool
	addr                string
	insecureListen      bool
	templatesDir        string
	stateDir            string
	storePath           string
	volumesDir          string
	healthTimeout       time.Duration
	idleTimeout         time.Duration
	retention           time.Duration
	reaperInterval      time.Duration
	eventRetention      time.Duration
	transportDownBudget time.Duration
	nodeDiskByte        int64
	nodeMemoryByte      int64
	sudo                bool
	viaUID              int
	publishIP           string
	shutdownTimeout     time.Duration
}

func newOrchestratorFlagSet(name string, handling flag.ErrorHandling, values *orchestratorFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, handling)
	fs.BoolVar(&values.showVersion, "version", false, "print the version and exit")
	fs.StringVar(&values.addr, "addr", "127.0.0.1:8080", "control-plane listen address; must be loopback unless -insecure-listen")
	fs.BoolVar(&values.insecureListen, "insecure-listen", false, "permit a non-loopback -addr even though the API has no authentication or TLS")
	fs.StringVar(&values.templatesDir, "templates", "/var/lib/khora/templates", "built Template root, with one TemplateID subdirectory per builtin")
	fs.StringVar(&values.stateDir, "state", "./scratch/state", "state root for overlays and runsc state and bundles")
	fs.StringVar(&values.storePath, "store", "./scratch/store.db", "SQLite store file")
	fs.StringVar(&values.volumesDir, "volumes-dir", "", "Volume storage root; empty uses state/volumes beside the overlay root")
	fs.DurationVar(&values.healthTimeout, "health-timeout", 30*time.Second, "cold-start readiness gate timeout")
	fs.DurationVar(&values.idleTimeout, "idle-timeout", 30*time.Minute, "default idle TTL before an idle running sandbox is paused")
	fs.DurationVar(&values.retention, "retention", 24*time.Hour, "default paused retention TTL before irreversible record and Overlay eviction; 0 disables eviction")
	fs.DurationVar(&values.reaperInterval, "reaper-interval", 30*time.Second, "reaper sweep cadence")
	fs.DurationVar(&values.eventRetention, "event-retention", 0, "delivered-event retention window; 0 retains the full outbox")
	fs.DurationVar(&values.transportDownBudget, "transport-down-budget", 2*time.Minute, "transport-down restart budget before a cold resume")
	fs.Int64Var(&values.nodeDiskByte, "node-disk-byte", 0, "node disk admission budget in bytes; 0 is unmetered and a positive value requires sandbox disk caps")
	fs.Int64Var(&values.nodeMemoryByte, "node-memory-byte", 0, "node memory admission budget in bytes; 0 is unmetered and a positive value requires sandbox memory caps")
	fs.BoolVar(&values.sudo, "sudo", false, "prefix runsc and ip with sudo for development")
	fs.IntVar(&values.viaUID, "via-uid", -1, "uid of the local second-hop process used by egress.via; unset refuses via declarations and the uid must differ from the orchestrator uid")
	fs.StringVar(&values.publishIP, "publish-ip", "", "host address for ingress.publish bindings; empty uses 127.0.0.1")
	fs.DurationVar(&values.shutdownTimeout, "shutdown-timeout", 30*time.Second, "maximum SIGTERM drain for in-flight requests; set it above the longest unary request")
	return fs
}
