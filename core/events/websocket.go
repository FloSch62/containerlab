package events

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gorilla/websocket"
)

const (
	defaultWebSocketBuffer = 16
	wsKeepaliveInterval    = 5 * time.Second
)

// WebSocketServer exposes aggregated events over a pull-based WebSocket API.
// Each connection can override snapshot and statistics settings via query
// parameters; events are streamed only when the client explicitly requests the
// next item to enforce backpressure.
type WebSocketServer struct {
	Address string
	Buffer  int
	Options Options

	upgrader websocket.Upgrader
}

var (
	errWSClientClosed     = errors.New("websocket client closed")
	errWSClientBufferFull = errors.New("websocket client buffer full")
)

// Listen starts the WebSocket server and blocks until ctx is canceled or the
// listener errors.
func (s *WebSocketServer) Listen(ctx context.Context) error {
	if strings.TrimSpace(s.Address) == "" {
		return fmt.Errorf("websocket server requires a listen address")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleConnection(ctx))

	server := &http.Server{
		Addr:    s.Address,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)

		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) || err == nil {
			return ctx.Err()
		}

		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || err == nil {
			return nil
		}

		return err
	}
}

func (s *WebSocketServer) handleConnection(ctx context.Context) http.HandlerFunc {
	upgrader := s.upgrader
	if upgrader.CheckOrigin == nil {
		upgrader.CheckOrigin = func(*http.Request) bool { return true }
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if err := ctx.Err(); err != nil {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)

			return
		}

		opts, err := s.optionsFromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Debugf("websocket upgrade failed: %v", err)

			return
		}

		client := newWSClient(conn, s.bufferSize(), opts)
		go client.serve(ctx)
	}
}

func (s *WebSocketServer) bufferSize() int {
	if s.Buffer > 0 {
		return s.Buffer
	}

	return defaultWebSocketBuffer
}

func (s *WebSocketServer) optionsFromRequest(r *http.Request) (Options, error) {
	opts := s.Options
	if opts.Format == "" {
		opts.Format = "json"
	}

	opts.Writer = nil
	opts.Sink = nil

	q := r.URL.Query()

	if v := q.Get("initial-state"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return opts, fmt.Errorf("invalid initial-state: %w", err)
		}

		opts.IncludeInitialState = parsed
	}

	if v := q.Get("interface-stats"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return opts, fmt.Errorf("invalid interface-stats: %w", err)
		}

		opts.IncludeInterfaceStats = parsed
	}

	if v := q.Get("interface-stats-interval"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return opts, fmt.Errorf("invalid interface-stats-interval: %w", err)
		}

		if parsed <= 0 {
			return opts, fmt.Errorf("interface-stats-interval must be positive")
		}

		opts.StatsInterval = parsed
	}

	return opts, nil
}

type wsClient struct {
	conn   *websocket.Conn
	buffer chan aggregatedEvent
	closed chan struct{}
	once   sync.Once
	opts   Options
	drops  atomic.Uint64
}

func newWSClient(conn *websocket.Conn, buffer int, opts Options) *wsClient {
	if buffer <= 0 {
		buffer = defaultWebSocketBuffer
	}

	return &wsClient{
		conn:   conn,
		buffer: make(chan aggregatedEvent, buffer),
		closed: make(chan struct{}),
		opts:   opts,
	}
}

func (c *wsClient) serve(parentCtx context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	events, runtimeErrs, cleanup, err := StreamChannel(ctx, c.opts)
	if err != nil {
		log.Errorf("failed to create event stream: %v", err)
		c.closeWithStatus(websocket.CloseInternalServerErr, "failed to start event stream")

		return
	}
	defer cleanup()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c.forwardEvents(ctx, events)
	}()

	go func() {
		defer wg.Done()
		c.forwardRuntimeErrors(ctx, runtimeErrs, cancel)
	}()

	c.run(ctx)
	cancel()
	wg.Wait()
}

func (c *wsClient) forwardEvents(ctx context.Context, events <-chan aggregatedEvent) {
	defer close(c.buffer)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}

			if err := c.enqueue(ev); err != nil {
				switch {
				case errors.Is(err, errWSClientClosed):
					return
				case errors.Is(err, errWSClientBufferFull):
					log.Debug("dropping event for slow websocket client")
				default:
					log.Debugf("failed to enqueue websocket event: %v", err)
				}
			}
		}
	}
}

func (c *wsClient) forwardRuntimeErrors(
	ctx context.Context,
	runtimeErrs <-chan error,
	cancel context.CancelFunc,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-runtimeErrs:
			if !ok {
				return
			}

			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warnf("runtime error in websocket stream: %v", err)
				c.closeWithStatus(websocket.CloseInternalServerErr, "runtime error")
				cancel()

				return
			}
		}
	}
}

func (c *wsClient) run(ctx context.Context) {
	defer c.close()

	c.conn.SetReadLimit(4096)
	c.conn.SetPongHandler(func(string) error { return nil })

	for {
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		if messageType != websocket.TextMessage {
			continue
		}

		if strings.TrimSpace(string(payload)) != "next" {
			continue
		}

		ev, ok := c.nextEvent(ctx)
		if !ok {
			c.closeWithStatus(websocket.CloseNormalClosure, "event stream finished")

			return
		}

		if err := c.writeEvent(ev); err != nil {
			log.Debugf("failed to write websocket event: %v", err)

			return
		}
	}
}

func (c *wsClient) nextEvent(ctx context.Context) (aggregatedEvent, bool) {
	if dropped := c.drops.Swap(0); dropped > 0 {
		return aggregatedEvent{
			Timestamp: time.Now(),
			Type:      "control",
			Action:    "buffer_drop",
			Attributes: map[string]string{
				"dropped_events": strconv.FormatUint(dropped, 10),
			},
		}, true
	}

	timer := time.NewTimer(wsKeepaliveInterval)
	defer timer.Stop()

	select {
	case ev, ok := <-c.buffer:
		if !ok {
			return aggregatedEvent{}, false
		}

		return ev, true
	case <-ctx.Done():
		return aggregatedEvent{}, false
	case <-c.closed:
		return aggregatedEvent{}, false
	case <-timer.C:
		return aggregatedEvent{
			Timestamp: time.Now(),
			Type:      "control",
			Action:    "keepalive",
		}, true
	}
}

func (c *wsClient) writeEvent(ev aggregatedEvent) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}

	if err := c.conn.WriteJSON(ev); err != nil {
		return err
	}

	return c.conn.SetWriteDeadline(time.Time{})
}

func (c *wsClient) closeWithStatus(code int, msg string) {
	deadline := time.Now().Add(5 * time.Second)
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg), deadline)
	c.close()
}

func (c *wsClient) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *wsClient) enqueue(ev aggregatedEvent) error {
	select {
	case <-c.closed:
		return errWSClientClosed
	default:
	}

	select {
	case c.buffer <- ev:
		return nil
	case <-c.closed:
		return errWSClientClosed
	default:
	}

	dropped := false
	select {
	case <-c.buffer:
		dropped = true
	default:
	}

	select {
	case c.buffer <- ev:
		if dropped {
			c.drops.Add(1)
		}

		return errWSClientBufferFull
	case <-c.closed:
		return errWSClientClosed
	}
}
