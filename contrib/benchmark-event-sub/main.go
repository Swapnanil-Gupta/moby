// benchmark-event-sub measures the latency of containerd's event subscription
// and notification path. It mimics what dockerd does for "docker wait":
//
//  1. Subscribes to containerd task events via gRPC streaming.
//  2. Creates a short-lived container (runs "true" or a configurable command).
//  3. Starts the task and records the wall-clock time.
//  4. Waits for the TaskExit event on the subscription stream.
//  5. Measures the delta between the exit timestamp reported by containerd
//     and the wall-clock time the event was received by this process.
//
// Usage:
//
//	sudo benchmark-event-sub [flags]
//
// Requires a running containerd daemon and root privileges.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	apievents "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	"github.com/containerd/typeurl/v2"
)

type result struct {
	Iteration    int           `json:"iteration"`
	ExitTS       time.Time     `json:"exit_ts"`
	ReceivedTS   time.Time     `json:"received_ts"`
	Latency      time.Duration `json:"latency_ns"`
	LatencyHuman string        `json:"latency"`
}

type summary struct {
	Iterations int     `json:"iterations"`
	MinMs      float64 `json:"min_ms"`
	MaxMs      float64 `json:"max_ms"`
	MeanMs     float64 `json:"mean_ms"`
	MedianMs   float64 `json:"median_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	StddevMs   float64 `json:"stddev_ms"`
}

var (
	flagAddr       = flag.String("address", "/run/containerd/containerd.sock", "containerd socket address")
	flagNamespace  = flag.String("namespace", "benchmark", "containerd namespace")
	flagIterations = flag.Int("iterations", 20, "number of containers to run")
	flagImage      = flag.String("image", "docker.io/library/alpine:latest", "OCI image reference")
	flagCmd        = flag.String("cmd", "true", "command to run inside the container (should exit quickly)")
	flagJSON       = flag.Bool("json", false, "output results as JSON")
)

func main() {
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "error: must run as root (containerd requires privileges)")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctx = namespaces.WithNamespace(ctx, *flagNamespace)

	client, err := containerd.New(*flagAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error connecting to containerd: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	image, err := ensureImage(ctx, client, *flagImage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error pulling image: %v\n", err)
		os.Exit(1)
	}

	// Subscribe to task events — same filter dockerd uses.
	eventStream, errC := client.EventService().Subscribe(ctx,
		"namespace=="+*flagNamespace+",topic~=|^/tasks/|",
	)

	type exitEvent struct {
		exitTS     time.Time
		receivedTS time.Time
	}
	var mu sync.Mutex
	exitEvents := make(map[string]exitEvent)
	exitNotify := make(map[string]chan struct{})

	// Event listener goroutine — mirrors dockerd's processEventStream.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errC:
				if err != nil {
					fmt.Fprintf(os.Stderr, "event stream error: %v\n", err)
				}
				return
			case ev := <-eventStream:
				received := time.Now()
				if ev.Event == nil {
					continue
				}
				v, err := typeurl.UnmarshalAny(ev.Event)
				if err != nil {
					continue
				}
				te, ok := v.(*apievents.TaskExit)
				if !ok {
					continue
				}
				// Only care about init process exit (same check as dockerd).
				if te.ID != te.ContainerID {
					continue
				}
				exitTS := protobuf.FromTimestamp(te.ExitedAt)
				mu.Lock()
				exitEvents[te.ContainerID] = exitEvent{exitTS: exitTS, receivedTS: received}
				if ch, ok := exitNotify[te.ContainerID]; ok {
					close(ch)
				}
				mu.Unlock()
			}
		}
	}()

	if !*flagJSON {
		fmt.Printf("Benchmarking containerd event subscription latency\n")
		fmt.Printf("  address:    %s\n", *flagAddr)
		fmt.Printf("  namespace:  %s\n", *flagNamespace)
		fmt.Printf("  image:      %s\n", *flagImage)
		fmt.Printf("  command:    %s\n", *flagCmd)
		fmt.Printf("  iterations: %d\n\n", *flagIterations)
	}

	results := make([]result, 0, *flagIterations)

	for i := range *flagIterations {
		containerID := fmt.Sprintf("bench-%d-%d", os.Getpid(), i)

		ch := make(chan struct{})
		mu.Lock()
		exitNotify[containerID] = ch
		mu.Unlock()

		if err := runOne(ctx, client, image, containerID, *flagCmd, ch); err != nil {
			fmt.Fprintf(os.Stderr, "iteration %d error: %v\n", i, err)
			cleanup(ctx, client, containerID)
			continue
		}

		mu.Lock()
		ev := exitEvents[containerID]
		mu.Unlock()

		latency := ev.receivedTS.Sub(ev.exitTS)
		res := result{
			Iteration:    i,
			ExitTS:       ev.exitTS,
			ReceivedTS:   ev.receivedTS,
			Latency:      latency,
			LatencyHuman: latency.String(),
		}
		results = append(results, res)

		if !*flagJSON {
			fmt.Printf("  [%3d] latency: %v\n", i, latency)
		}

		cleanup(ctx, client, containerID)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no successful iterations")
		os.Exit(1)
	}

	s := computeSummary(results)

	if *flagJSON {
		out := struct {
			Results []result `json:"results"`
			Summary summary  `json:"summary"`
		}{results, s}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	} else {
		fmt.Printf("\n--- Summary (%d iterations) ---\n", s.Iterations)
		fmt.Printf("  min:    %.3f ms\n", s.MinMs)
		fmt.Printf("  max:    %.3f ms\n", s.MaxMs)
		fmt.Printf("  mean:   %.3f ms\n", s.MeanMs)
		fmt.Printf("  median: %.3f ms\n", s.MedianMs)
		fmt.Printf("  p95:    %.3f ms\n", s.P95Ms)
		fmt.Printf("  p99:    %.3f ms\n", s.P99Ms)
		fmt.Printf("  stddev: %.3f ms\n", s.StddevMs)
	}
}

func ensureImage(ctx context.Context, client *containerd.Client, ref string) (containerd.Image, error) {
	image, err := client.GetImage(ctx, ref)
	if err == nil {
		return image, nil
	}
	return client.Pull(ctx, ref, containerd.WithPullUnpack)
}

// runOne creates a container, starts it, and blocks until the exit event
// arrives on the subscription stream — mimicking dockerd's wait path.
func runOne(ctx context.Context, client *containerd.Client, image containerd.Image, id, cmd string, exitCh <-chan struct{}) error {
	ctr, err := client.NewContainer(ctx, id,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(id+"-snap", image),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/sh", "-c", cmd),
		),
	)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	task, err := ctr.NewTask(ctx, cio.NullIO)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("start task: %w", err)
	}

	// Block until the exit event arrives on our subscription stream.
	// This is the equivalent of <-waitC in dockerd's postContainersWait.
	select {
	case <-exitCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout waiting for exit event")
	}

	return nil
}

func cleanup(ctx context.Context, client *containerd.Client, id string) {
	ctr, err := client.LoadContainer(ctx, id)
	if err != nil {
		return
	}
	task, err := ctr.Task(ctx, nil)
	if err == nil {
		task.Delete(ctx, containerd.WithProcessKill)
	}
	ctr.Delete(ctx, containerd.WithSnapshotCleanup)
}

func computeSummary(results []result) summary {
	latencies := make([]float64, len(results))
	for i, r := range results {
		latencies[i] = float64(r.Latency.Nanoseconds()) / 1e6
	}
	sort.Float64s(latencies)

	n := float64(len(latencies))
	var sum float64
	for _, v := range latencies {
		sum += v
	}
	mean := sum / n

	var variance float64
	for _, v := range latencies {
		d := v - mean
		variance += d * d
	}
	variance /= n

	return summary{
		Iterations: len(results),
		MinMs:      latencies[0],
		MaxMs:      latencies[len(latencies)-1],
		MeanMs:     mean,
		MedianMs:   percentile(latencies, 50),
		P95Ms:      percentile(latencies, 95),
		P99Ms:      percentile(latencies, 99),
		StddevMs:   math.Sqrt(variance),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p / 100 * float64(len(sorted)-1)
	lower := int(idx)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
