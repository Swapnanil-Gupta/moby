package daemon

import (
	"context"
	"fmt"

	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/moby/v2/daemon/internal/otelutil"
	"github.com/moby/moby/v2/daemon/pkg/oci"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// initializeCreatedTask performs any initialization that needs to be done to
// prepare a freshly-created task to be started.
func (daemon *Daemon) initializeCreatedTask(
	ctx context.Context,
	cfg *config.Config,
	tsk types.Task,
	ctr *container.Container,
	spec *specs.Spec,
) (retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, "daemon.initializeCreatedTask", trace.WithAttributes(
		attribute.String("container.ID", ctr.ID),
	))
	defer func() {
		otelutil.RecordStatus(span, retErr)
		span.End()
	}()

	if ctr.Config.NetworkDisabled {
		return nil
	}
	nspath, ok := oci.NamespacePath(spec, specs.NetworkNamespace)
	if ok && nspath == "" { // the runtime has been instructed to create a new network namespace for tsk.
		sb, err := daemon.netController.GetSandbox(ctr.ID)
		if err != nil {
			return errdefs.System(err)
		}
		if err := sb.SetKey(ctx, fmt.Sprintf("/proc/%d/ns/net", tsk.Pid())); err != nil {
			return errdefs.System(err)
		}
	}
	if err := daemon.allocateNetwork(ctx, cfg, ctr); err != nil {
		return fmt.Errorf("%s: %w", errSetupNetworking, err)
	}
	return nil
}
