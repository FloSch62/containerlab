# events command

## Description

The `events` command streams lifecycle updates for every Containerlab resource and augments them with interface change notifications collected from the container network namespaces. The output combines the selected runtime's event feed (for example Docker) with the netlink information that powers `containerlab inspect interfaces`, so you can observe container activity and interface state changes in real time without selecting a specific lab.

## Usage

`containerlab [global-flags] events [local-flags]`

**aliases:** `ev`

The command respects the global flags such as `--runtime`, `--debug`, or `--log-level`. It adds local options:

- `--format` controls the output representation (`plain`, `json`).
- `--initial-state` emits a snapshot of currently running containers and their interface states before following live updates.
- `--interface-stats` enables periodic interface counter sampling; leave unset to report only lifecycle and state changes.
- `--interface-stats-interval` customizes how frequently statistics are collected (for example `500ms`, `2s`, `1m`).
- `--websocket-listen` starts a WebSocket server alongside the CLI stream so other tools can consume aggregated events.
- `--websocket-buffer` sets the per-connection queue depth before slow WebSocket clients start dropping events.

When invoked with no arguments it discovers all running labs and immediately begins streaming events; new labs that start after the command begins are picked up automatically.
When `--websocket-listen` is provided the CLI switches to server mode and no longer prints events to stdout; clients must connect over WebSocket to receive data.

## Event format

In the default `plain` format every line mirrors the `docker events` format:

```
<timestamp> <type> <action> <actor> (<key>=<value>, ...)
```

- **Runtime events** show the short container ID as the actor and include the original attributes supplied by the container runtime (for example `image`, `name`, `containerlab`, `scope`, …). When `--initial-state` is enabled the stream starts with `container <state>` snapshots (for example `container running`) that carry an `origin=snapshot` attribute.
- **Interface events** use type `interface` and `origin=netlink` in the attribute list. They also report interface-specific data such as `ifname`, `state`, `mtu`, `mac`, `type`, `alias`, and the lab label. The actor is still the container short ID, and the container name is supplied in the attributes (`name=...`).
- Interface notifications are emitted when a link appears, disappears, or when its relevant properties (operational state, MTU, alias, MAC address, type) change. Initial snapshots use the `snapshot` action when `--initial-state` is requested. When interface statistics are enabled the stream also includes `interface stats` updates with byte/packet counters and rate estimates.

When `--format json` is used, each event becomes a single JSON object on its own line. The fields match the plain output (`timestamp`, `type`, `action`, `actor_id`, `actor_name`, `actor_full_id`) and include an `attributes` map with the same key/value pairs that the plain formatter prints.

## Examples

### Watch an existing lab and new deployments

```
$ containerlab events
2024-07-01T11:02:56.123456000Z container start 5d0b5a9ad3f1 (containerlab=frr-lab, image=ghcr.io/srl-labs/frr, name=clab-frr-lab-frr01)
2024-07-01T11:02:57.004321000Z interface create 5d0b5a9ad3f1 (ifname=eth0, index=22, lab=frr-lab, mac=02:42:ac:14:00:02, mtu=1500, name=clab-frr-lab-frr01, origin=netlink, state=up, type=veth)
2024-07-01T11:02:57.104512000Z interface update 5d0b5a9ad3f1 (ifname=eth0, index=22, lab=frr-lab, mac=02:42:ac:14:00:02, mtu=9000, name=clab-frr-lab-frr01, origin=netlink, state=up, type=veth)
2024-07-01T11:05:12.918273000Z container die 5d0b5a9ad3f1 (containerlab=frr-lab, exitCode=0, image=ghcr.io/srl-labs/frr, name=clab-frr-lab-frr01)
2024-07-01T11:05:13.018456000Z interface delete 5d0b5a9ad3f1 (ifname=eth0, index=22, lab=frr-lab, name=clab-frr-lab-frr01, origin=netlink, state=up, type=veth)
```

The stream contains all currently running labs and stays active to capture subsequent deployments, restarts, or interface adjustments.

### Include existing resources in the stream

```
$ containerlab events --initial-state
2024-07-01T11:02:55.912345000Z container running 5d0b5a9ad3f1 (containerlab=frr-lab, image=ghcr.io/srl-labs/frr, name=clab-frr-lab-frr01, origin=snapshot, state=running)
2024-07-01T11:02:55.912678000Z interface snapshot 5d0b5a9ad3f1 (ifname=eth0, index=22, lab=frr-lab, mac=02:42:ac:14:00:02, mtu=1500, name=clab-frr-lab-frr01, origin=netlink, state=up, type=veth)
…
```

This mode begins with a point-in-time view of every running container and interface before switching to live updates.

### Include interface statistics

```
containerlab events --interface-stats
```

Statistics are disabled by default. Enabling them augments the feed with periodic counter samples in addition to lifecycle and state changes. Use `--interface-stats-interval` to balance fidelity with overhead: values between `1s` and `5s` work well for most labs, while larger deployments may prefer longer intervals (for example `10s`) to avoid excessive sampling load.

### Consume events over WebSocket

```bash
$ sudo containerlab events --websocket-listen 0.0.0.0:8081 --interface-stats --interface-stats-interval 500ms
```

When `--websocket-listen` is provided the command stops printing events to the terminal and instead upgrades incoming WebSocket connections on the specified address. Each client must send a `next` text frame to receive the next aggregated event in JSON form, allowing consumers to drive the pace of the stream and apply their own backpressure. The server keeps a bounded queue per connection (see `--websocket-buffer`); when that queue fills because a client is too slow, the oldest events are dropped for that client while the connection stays up. Multiple WebSocket clients can connect simultaneously and pull at their own cadence.

```bash
$ sudo containerlab events --websocket-listen 0.0.0.0:8081 --websocket-buffer 64
```

Increasing the buffer gives slower consumers more headroom before their stream starts dropping events, which is helpful when pairing the WebSocket feed with high-frequency producers or when clients temporarily pause between `next` requests.

Clients can override snapshot and statistics settings on a per-connection basis with query parameters:

- `initial-state=true` requests the initial container/interface snapshot for that connection.
- `interface-stats=true` enables interface counter sampling for the client, and
- `interface-stats-interval=500ms` (or any Go duration) controls the sampling cadence.

Whenever the server drops events for a client because its buffer is full, it injects a `control buffer_drop` notification into that client's stream (delivered before the next real event) with the number of samples that were lost. If no events are available for roughly five seconds, the server sends a `control keepalive` placeholder so clients with read deadlines can stay connected. Consumers can monitor these signals and adjust their pacing or buffer size accordingly.

This mode pairs well with high-frequency interface statistics because event delivery is explicitly paced by the client. Scripts can request updates only when they are ready to process them, avoiding buffer overruns while still capturing fine-grained counter changes. Combine it with a larger `--websocket-buffer` when sampling very frequently to absorb bursts without losing intermediate samples.

#### Example WebSocket client

The repository ships with a tiny Go example that illustrates how to connect to the WebSocket endpoint, request the next event, and decode the JSON payload. Build it with `go build` and supply the address exposed by `containerlab events`:

```bash
$ go build ./examples/events/wsclient
$ ./wsclient --addr ws://127.0.0.1:8081 --initial-state --interface-stats --interface-stats-interval 500ms --pace 1s
2024-07-01T11:02:56.123456000Z container start 5d0b5a9ad3f1 (attributes: map[containerlab:frr-lab image:ghcr.io/srl-labs/frr name:clab-frr-lab-frr01])
...
```

The optional `--pace` flag inserts a delay after each received event before the client asks for the next one; setting it to zero (the default) requests updates immediately, while positive durations (for example `100ms`, `1s`) demonstrate how slow consumers behave. Other flags map to the WebSocket query parameters described above, making it easy to request snapshots or change the statistics interval without restarting the server. For each iteration the client reads and parses an event, optionally waits, and then sends another `next` control message.

### Use with alternative runtimes

Containerlab streams events from the runtime selected via the global `--runtime` flag.

> **Currently supported runtime:** `docker`  
> Runtimes that do not implement the `events` API (or are not yet supported by Containerlab) will exit with an explanatory error.

## See also

- [`inspect interfaces`](inspect/interfaces.md) – produces a point-in-time view of the same interface details that `events` reports continuously.
- `docker events` – the raw runtime feed that Containerlab builds upon.
