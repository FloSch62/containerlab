package events

import (
	"io"
	"time"

	clabcore "github.com/srl-labs/containerlab/core"
)

// Options configure how runtime and interface events are sourced and rendered.
type Options struct {
	Format                string
	Runtime               string
	IncludeInitialState   bool
	IncludeInterfaceStats bool
	StatsInterval         time.Duration
	ClabOptions           []clabcore.ClabOption
	Writer                io.Writer
	Sink                  EventSink
}

func (o Options) writer() io.Writer {
	if o.Writer != nil {
		return o.Writer
	}

	return io.Discard
}

func (o Options) sink() (EventSink, error) {
	if o.Sink != nil {
		return o.Sink, nil
	}

	return NewWriterSink(o.Format, o.writer())
}
