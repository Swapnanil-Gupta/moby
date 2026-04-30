# benchmark-event-sub

Benchmarks the latency of containerd's gRPC event subscription and notification
path — the same mechanism dockerd uses to detect that a container has stopped
(i.e., the `docker wait` code path).

## What it measures

For each iteration the tool:

1. Subscribes to containerd's task event stream (same filter as dockerd:
   `topic~=|^/tasks/|`).
2. Creates a short-lived container and starts it.
3. Blocks on the subscription stream until a `TaskExit` event arrives (mirrors
   `<-waitC` in dockerd's `postContainersWait`).
4. Records **latency** = `time.Now()` (when the event was received in this
   process) minus the `ExitedAt` timestamp reported by containerd in the event.

This captures the end-to-end delay through:
- The containerd shim detecting process exit (`waitpid`)
- The shim publishing the event to containerd
- containerd delivering it over the gRPC streaming subscription
- Go receiving and unmarshalling the protobuf envelope

## Build

```sh
cd contrib/benchmark-event-sub
go build -o benchmark-event-sub .
```

## Usage

Requires a running containerd daemon and root privileges.

```sh
# Default: 20 iterations with alpine running "true"
sudo ./benchmark-event-sub

# Custom iterations and command
sudo ./benchmark-event-sub -iterations 100 -cmd "echo hello"

# JSON output for scripting
sudo ./benchmark-event-sub -iterations 50 -json | jq .summary

# Custom containerd socket
sudo ./benchmark-event-sub -address /run/containerd/containerd.sock
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-address` | `/run/containerd/containerd.sock` | containerd socket path |
| `-namespace` | `benchmark` | containerd namespace to use |
| `-iterations` | `20` | number of container runs |
| `-image` | `docker.io/library/alpine:latest` | OCI image reference |
| `-cmd` | `true` | command to run (should exit quickly) |
| `-json` | `false` | output results as JSON |

## Example output

```
Benchmarking containerd event subscription latency
  address:    /run/containerd/containerd.sock
  namespace:  benchmark
  image:      docker.io/library/alpine:latest
  command:    true
  iterations: 20

  [  0] latency: 1.234ms
  [  1] latency: 0.987ms
  ...

--- Summary (20 iterations) ---
  min:    0.654 ms
  max:    2.345 ms
  mean:   1.123 ms
  median: 1.089 ms
  p95:    1.876 ms
  p99:    2.234 ms
  stddev: 0.345 ms
```
