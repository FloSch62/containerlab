package cmd

import (
	"github.com/spf13/cobra"
	clabevents "github.com/srl-labs/containerlab/core/events"
	clabutils "github.com/srl-labs/containerlab/utils"
)

func eventsCmd(o *Options) (*cobra.Command, error) {
	c := &cobra.Command{
		Use:     "events",
		Short:   "stream lab lifecycle and interface events",
		Long:    "stream container runtime events and interface updates for all running labs using the selected runtime\n" + "reference: https://containerlab.dev/cmd/events/",
		Aliases: []string{"ev"},
		PreRunE: func(*cobra.Command, []string) error {
			return clabutils.CheckAndGetRootPrivs()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return eventsFn(cmd, o)
		},
	}

	c.Flags().StringVarP(
		&o.Events.Format,
		"format",
		"f",
		o.Events.Format,
		"output format. One of [plain, json]",
	)

	c.Flags().BoolVarP(
		&o.Events.IncludeInitialState,
		"initial-state",
		"i",
		o.Events.IncludeInitialState,
		"emit the current container and interface states before streaming new events",
	)

	c.Flags().BoolVar(
		&o.Events.IncludeInterfaceStats,
		"interface-stats",
		o.Events.IncludeInterfaceStats,
		"include interface statistics updates when streaming events",
	)

	c.Flags().DurationVar(
		&o.Events.StatsInterval,
		"interface-stats-interval",
		o.Events.StatsInterval,
		"interval between interface statistics samples (requires --interface-stats)",
	)

	c.Flags().StringVar(
		&o.Events.WebsocketListen,
		"websocket-listen",
		o.Events.WebsocketListen,
		"serve events via WebSocket on the provided address (for example 0.0.0.0:8081)",
	)

	c.Flags().IntVar(
		&o.Events.WebsocketBuffer,
		"websocket-buffer",
		o.Events.WebsocketBuffer,
		"per-connection buffer for pending events before disconnecting slow consumers",
	)

	c.Example = `# Stream container and interface events in plain text
containerlab events

# Stream events as JSON
containerlab events --format json`

	return c, nil
}

func eventsFn(cmd *cobra.Command, o *Options) error {
	opts := clabevents.Options{
		Format:                o.Events.Format,
		Runtime:               o.Global.Runtime,
		IncludeInitialState:   o.Events.IncludeInitialState,
		IncludeInterfaceStats: o.Events.IncludeInterfaceStats,
		StatsInterval:         o.Events.StatsInterval,
		ClabOptions:           o.ToClabOptions(),
		Writer:                cmd.OutOrStdout(),
	}

	if o.Events.WebsocketListen != "" {
		socketOpts := opts
		socketOpts.Writer = nil
		socketOpts.Sink = nil
		socketOpts.Format = "json"

		server := &clabevents.WebSocketServer{
			Address: o.Events.WebsocketListen,
			Buffer:  o.Events.WebsocketBuffer,
			Options: socketOpts,
		}

		return server.Listen(cmd.Context())
	}

	return clabevents.Stream(cmd.Context(), opts)
}
