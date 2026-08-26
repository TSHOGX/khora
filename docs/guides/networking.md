# Configure networking

## Read before editing

Egress changes replace the complete declaration. Use this sequence so a concurrent
publish is not lost:

```sh
./bin/khora sandbox pause sbx_...
./bin/khora network get sbx_... > network.json
# edit network.json
./bin/khora network patch sbx_... --json "$(<network.json)"
./bin/khora sandbox resume sbx_...
```

Read-only members returned by `network get` are accepted and ignored by the HTTP
write surface, so the response is a valid read-modify-write body. That surface
rejects other unknown members. The CLI first decodes `--json` into its typed Network
value, so it does not preserve unknown members for server-side validation.

## Allow selected egress

This declaration denies unmatched traffic while allowing HTTPS to one prefix and
DNS to one resolver:

```json
{
  "egress": {
    "default": "deny",
    "allow": [
      {"cidr":"192.0.2.0/24","port":443,"proto":"tcp"},
      {"cidr":"203.0.113.53/32","port":53,"proto":"udp"}
    ],
    "via": []
  },
  "ingress": {"publish": []}
}
```

CIDRs are IP literals. Omitted CIDR means any destination; omitted port means every
port. `proto` is always required. The `bare` Template contains no DNS configuration;
supply `/etc/resolv.conf` and a matching UDP/53 rule when name resolution is needed.

## Steer TCP through a second hop

`via[]` maps matched TCP flows to a host-local endpoint:

```json
{
  "match": [{"cidr":"198.51.100.0/24","port":443,"proto":"tcp"}],
  "endpoint": "127.0.0.1:15001"
}
```

The endpoint process is outside Khora. The engine does not start it or provide its
authentication. Placement fails closed if the second-hop restriction cannot be
installed. UDP is not accepted in `via[]`.

## Publish a guest port

The guest service must bind its sandbox interface, normally `0.0.0.0`, rather than
only `127.0.0.1`:

```sh
./bin/khora network publish sbx_... --guest-port 8080 --proto tcp
```

For a running Sandbox, the returned `host_endpoint` is usable immediately and
`applies_at` is `now`. For a paused Sandbox, the declaration is persisted,
`host_endpoint` is absent, and `applies_at` is `next-resume`. Repeating the same
guest port and protocol is idempotent.

Packet counters are placement-local diagnostics. Their absence means no readable
live rule; zero does not prove that a rule was never hit. Counters reset when rules
are reinstalled.
