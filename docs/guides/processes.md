# Run and reconnect to processes

Processes have durable opaque handles scoped to a Sandbox. A guest PID is only
a locator inside one Placement epoch. Pause ends the current locator; the
logical record retains an exited result for the configured retention window.

## Start a command

```sh
./bin/khora process start --sandbox sbx_... /usr/bin/env \
  --arg=sh --arg=-c --arg='pwd; id' \
  --cwd /tmp --env TERM=xterm-256color
```

The command streams stdout, stderr, and exit events until the process ends. Use
both `--pty-rows` and `--pty-cols` for a PTY. Without them, stdout and stderr remain
distinct streams. The process service contract is defined in
[proto/khora/transport/v1/transport.proto](../../proto/khora/transport/v1/transport.proto).

## Reconnect and fan out

List live processes, then connect by handle (the PID form remains for legacy
clients):

```sh
./bin/khora process list --sandbox sbx_...
./bin/khora process connect --sandbox sbx_... proc_...
```

`process list` scans the guest's `/proc` and includes logical handles. A connection
first replays the bounded output tail, then receives live output. Every output
frame carries a sequence and Placement epoch; if the 64 KiB tail cannot cover the
requested cursor, the stream emits an explicit overflow/reset marker and returns a
resource-exhausted error so the caller can rescan. Subscriber backpressure is
isolated so one slow reader does not block the child or other readers. The behavior is covered by
[cmd/sandboxd/process_test.go](../../cmd/sandboxd/process_test.go) and
[test/slow_subscriber_substrate_test.go](../../test/slow_subscriber_substrate_test.go).

## Send input or a signal

```sh
printf 'status\n' | ./bin/khora process input --sandbox sbx_... 42
./bin/khora process signal --sandbox sbx_... --signal 15 42
```

Signals, input, resize, and stdin close accept the opaque handle plus an expected
Placement epoch; stale epochs fail closed. The PID forms remain supported for
compatibility. Closing stdin is available through the Connect-RPC `CloseStdin` method. Inspect the generated
[CLI reference](../api/cli.md) for the exact flags supported by the current binary.

Transport unavailability is distinct from a missing process. A paused Sandbox has
no data plane and produces a state conflict; an unreachable `sandboxd` is reported
as service unavailable. See [Errors](../api/errors-and-events.md).
