package container

import (
	"context"
	"time"

	"github.com/containerd/log"
	"go.opentelemetry.io/otel"
)

const (
	loggerCloseTimeout = 10 * time.Second
)

// Reset puts a container into a state where it can be restarted again.
//
// Callers are expected to obtain a lock on the container.
func (container *Container) Reset() {
	_, span := otel.Tracer("").Start(context.Background(), "container.Reset")
	defer span.End()

	{
		_, closeSpan := otel.Tracer("").Start(context.Background(), "container.Reset.closeStreams")
		if err := container.CloseStreams(); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"container": container.ID,
				"error":     err,
			}).Error("failed to close container streams")
		}
		closeSpan.End()
	}

	// Re-create a brand new stdin pipe once the container exited
	if container.Config.OpenStdin {
		container.StreamConfig.NewInputPipes()
	}

	if container.LogDriver == nil {
		span.AddEvent("logDriver.nil")
		return
	}

	if container.LogCopier != nil {
		_, copierSpan := otel.Tracer("").Start(context.Background(), "container.Reset.logCopierWait")
		exit := make(chan struct{})
		go func() {
			container.LogCopier.Wait()
			close(exit)
		}()

		timer := time.NewTimer(loggerCloseTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			copierSpan.AddEvent("timeout")
			log.G(context.TODO()).WithFields(log.Fields{
				"container": container.ID,
			}).Warn("logger didn't exit in time: logs may be truncated")
		case <-exit:
		}
		copierSpan.End()
	}

	{
		_, logCloseSpan := otel.Tracer("").Start(context.Background(), "container.Reset.logDriverClose")
		if err := container.LogDriver.Close(); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"container": container.ID,
				"error":     err,
			}).Warn("error closing log driver")
		}
		logCloseSpan.End()
	}
	container.LogCopier = nil
	container.LogDriver = nil
}
