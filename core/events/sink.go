package events

import (
	"context"
	"errors"
	"io"

	"github.com/charmbracelet/log"
	"golang.org/x/sync/errgroup"
)

// EventSink consumes aggregated events emitted by Stream and related helpers.
type EventSink interface {
	Consume(ctx context.Context, events <-chan aggregatedEvent) error
}

// EventSinkFunc adapts ordinary functions to the EventSink interface.
type EventSinkFunc func(ctx context.Context, events <-chan aggregatedEvent) error

// Consume executes f(ctx, events).
func (f EventSinkFunc) Consume(ctx context.Context, events <-chan aggregatedEvent) error {
	return f(ctx, events)
}

// NewWriterSink returns an EventSink that formats events and writes them to w.
func NewWriterSink(format string, w io.Writer) (EventSink, error) {
	formatter, err := newFormatter(format, w)
	if err != nil {
		return nil, err
	}

	return EventSinkFunc(func(ctx context.Context, events <-chan aggregatedEvent) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-events:
				if !ok {
					return nil
				}

				if err := formatter(ev); err != nil {
					log.Debugf("failed to write event: %v", err)
				}
			}
		}
	}), nil
}

// MultiSink fans events out to all provided sinks.
func MultiSink(sinks ...EventSink) EventSink {
	filtered := make([]EventSink, 0, len(sinks))
	for _, sink := range sinks {
		if sink != nil {
			filtered = append(filtered, sink)
		}
	}

	if len(filtered) == 0 {
		return EventSinkFunc(func(ctx context.Context, events <-chan aggregatedEvent) error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case _, ok := <-events:
					if !ok {
						return nil
					}
				}
			}
		})
	}

	return EventSinkFunc(func(ctx context.Context, events <-chan aggregatedEvent) error {
		g, gctx := errgroup.WithContext(ctx)

		type pipe struct {
			ch chan aggregatedEvent
		}

		pipes := make([]pipe, len(filtered))
		for idx, sink := range filtered {
			ch := make(chan aggregatedEvent)
			pipes[idx] = pipe{ch: ch}

			sink := sink
			g.Go(func() error {
				defer close(ch)

				return sink.Consume(gctx, ch)
			})
		}

		broadcast := func(ev aggregatedEvent) error {
			for _, pipe := range pipes {
				select {
				case pipe.ch <- ev:
				case <-gctx.Done():
					return gctx.Err()
				}
			}

			return nil
		}

		drainAndClose := func() error {
			for _, pipe := range pipes {
				close(pipe.ch)
			}

			if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}

			return nil
		}

		for {
			select {
			case <-gctx.Done():
				if err := drainAndClose(); err != nil {
					return err
				}

				if err := gctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
					return err
				}

				return nil
			case ev, ok := <-events:
				if !ok {
					return drainAndClose()
				}

				if err := broadcast(ev); err != nil && !errors.Is(err, context.Canceled) {
					if derr := drainAndClose(); derr != nil {
						return derr
					}

					return err
				}
			}
		}
	})
}
