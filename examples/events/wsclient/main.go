package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
)

const wsDeadline = 10 * time.Second

type event struct {
	Timestamp   time.Time         `json:"timestamp"`
	Type        string            `json:"type"`
	Action      string            `json:"action"`
	ActorID     string            `json:"actor_id"`
	ActorName   string            `json:"actor_name"`
	ActorFullID string            `json:"actor_full_id"`
	Attributes  map[string]string `json:"attributes"`
}

func main() {
	addr := flag.String("addr", "ws://127.0.0.1:8081", "WebSocket endpoint exposed by containerlab events")
	pace := flag.Duration("pace", 0, "Interval between event requests; zero requests the next event immediately")
	initialState := flag.Bool("initial-state", false, "Request the initial snapshot of containers and interfaces")
	interfaceStats := flag.Bool("interface-stats", false, "Request interface statistics updates")
	statsInterval := flag.Duration("interface-stats-interval", time.Second, "Interval between interface statistics samples")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	u, err := url.Parse(*addr)
	if err != nil {
		log.Fatalf("invalid addr: %v", err)
	}

	query := u.Query()
	if *initialState {
		query.Set("initial-state", "true")
	}

	if *interfaceStats {
		query.Set("interface-stats", "true")
		query.Set("interface-stats-interval", statsInterval.String())
	}

	u.RawQuery = query.Encode()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			log.Fatalf("dial failed with status %s: %v", resp.Status, err)
		}

		log.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()

		deadline := time.Now().Add(2 * time.Second)
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client exiting"),
			deadline,
		)
		conn.Close()
	}()

	withDeadline := func(fn func() error) error {
		if err := fn(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() && ctx.Err() != nil {
				return ctx.Err()
			}

			return err
		}

		return nil
	}

	requestNext := func() error {
		return withDeadline(func() error {
			if err := conn.SetWriteDeadline(time.Now().Add(wsDeadline)); err != nil {
				return err
			}

			if err := conn.WriteMessage(websocket.TextMessage, []byte("next")); err != nil {
				return err
			}

			return conn.SetWriteDeadline(time.Time{})
		})
	}

	recv := func(ev *event) error {
		return withDeadline(func() error {
			if err := conn.SetReadDeadline(time.Now().Add(wsDeadline)); err != nil {
				return err
			}

			_, payload, err := conn.ReadMessage()
			if err != nil {
				return err
			}

			if err := json.Unmarshal(payload, ev); err != nil {
				return err
			}

			return conn.SetReadDeadline(time.Time{})
		})
	}

	waitForPace := func() bool {
		if *pace <= 0 {
			select {
			case <-ctx.Done():
				return false
			default:
				return true
			}
		}

		timer := time.NewTimer(*pace)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}

	if err := requestNext(); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Fatalf("failed to request next event: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var ev event
		if err := recv(&ev); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Fatalf("failed to read event: %v", err)
		}

		fmt.Printf("%s %s %s %s (attributes: %v)\n", ev.Timestamp.Format(time.RFC3339Nano), ev.Type, ev.Action, ev.ActorID, ev.Attributes)

		if !waitForPace() {
			return
		}

		if err := requestNext(); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Fatalf("failed to request next event: %v", err)
		}
	}
}
