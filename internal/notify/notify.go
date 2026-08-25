// Package notify delivers watch notifications.
//
// The daemon publishes to MQTT and stops there. That is deliberate: MQTT is a
// bus rather than a destination, so Web Push, ntfy, Telegram or e-mail become
// subscribers to it rather than changes to this package.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/deviationist/scootless/internal/poll"
)

// Payload is what a subscriber receives when a watch fires. It is a distinct
// type from the internal notification so that the wire format is something we
// choose and can keep stable, rather than whatever the internals happen to
// look like today.
type Payload struct {
	WatchID  string    `json:"watch_id"`
	Kind     string    `json:"kind"`
	FiredAt  time.Time `json:"fired_at"`
	Fence    Fence     `json:"fence"`
	Count    int       `json:"count"`
	Vehicles []Vehicle `json:"vehicles"`
}

// Fence identifies where the watch was looking, without disclosing the exact
// coordinates: a subscriber needs to know which watch fired, not where the
// person lives.
type Fence struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	RadiusM int    `json:"radius_m"`
}

// Vehicle is one scooter worth walking to.
type Vehicle struct {
	ID         string   `json:"id"`
	Operator   string   `json:"operator"`
	DistanceM  int      `json:"distance_m"`
	Bearing    string   `json:"bearing"`
	RangeKM    float64  `json:"range_km"`
	BatteryPct *float64 `json:"battery_pct"`
	AppLink    string   `json:"app_link,omitempty"`
}

// PayloadOf converts an internal notification into the wire format.
func PayloadOf(n poll.Notification) Payload {
	p := Payload{
		WatchID: n.WatchID,
		Kind:    string(n.Kind),
		FiredAt: n.FiredAt.UTC(),
		Fence:   Fence{ID: n.Fence.ID, Name: n.Fence.Name, RadiusM: n.Fence.RadiusM},
		Count:   n.Count,
	}
	for _, v := range n.Vehicles {
		p.Vehicles = append(p.Vehicles, Vehicle{
			ID:         v.ID,
			Operator:   v.Operator,
			DistanceM:  int(v.DistanceM + 0.5),
			Bearing:    v.Compass(),
			RangeKM:    float64(v.RangeM) / 1000,
			BatteryPct: v.FuelPct,
			AppLink:    v.AppLinkIOS,
		})
	}
	return p
}

// Publisher is the slice of an MQTT client this package needs, so tests do not
// require a broker.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload any) mqtt.Token
	IsConnected() bool
	Disconnect(quiesce uint)
}

// MQTT publishes notifications to a broker.
type MQTT struct {
	client Publisher
	prefix string
	log    *slog.Logger

	mu sync.Mutex
}

// Options configures the MQTT sink.
type Options struct {
	Broker   string // e.g. tcp://host:1883
	ClientID string
	Username string
	Password string
	Prefix   string // topic prefix, default "scootless"
	Log      *slog.Logger
}

// Dial connects to the broker and returns a sink.
func Dial(ctx context.Context, o Options) (*MQTT, error) {
	if o.Broker == "" {
		return nil, fmt.Errorf("no broker configured")
	}
	opts := mqtt.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(clientID(o.ClientID)).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectTimeout(10 * time.Second).
		SetMaxReconnectInterval(time.Minute)
	if o.Username != "" {
		opts.SetUsername(o.Username)
		opts.SetPassword(o.Password)
	}
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, fmt.Errorf("connecting to %s: timed out", o.Broker)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", o.Broker, err)
	}
	return New(c, o.Prefix, o.Log), nil
}

// New wraps an existing client. Exported so a caller - or a test - can supply
// its own.
func New(c Publisher, prefix string, log *slog.Logger) *MQTT {
	if prefix == "" {
		prefix = "scootless"
	}
	if log == nil {
		log = slog.Default()
	}
	return &MQTT{client: c, prefix: strings.TrimSuffix(prefix, "/"), log: log}
}

// Publish sends a fired notification.
func (m *MQTT) Publish(ctx context.Context, n poll.Notification) error {
	body, err := json.Marshal(PayloadOf(n))
	if err != nil {
		return err
	}
	topic := fmt.Sprintf("%s/watch/%s/fired", m.prefix, n.WatchID)
	return m.publish(ctx, topic, body)
}

// Sample publishes the current count for a fence, for anything that wants to
// watch the numbers rather than wait for a threshold.
func (m *MQTT) Sample(ctx context.Context, fenceID string, counts map[string]int, at time.Time) error {
	body, err := json.Marshal(map[string]any{
		"fence_id": fenceID,
		"at":       at.UTC(),
		"counts":   counts,
	})
	if err != nil {
		return err
	}
	return m.publish(ctx, fmt.Sprintf("%s/fence/%s/sample", m.prefix, fenceID), body)
}

func (m *MQTT) publish(ctx context.Context, topic string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// QoS 1: a missed "your scooter is here" is the one message that must not
	// be dropped, and a duplicate is merely mildly annoying. Not retained -
	// this notification is worthless to a subscriber that connects later.
	tok := m.client.Publish(topic, 1, false, body)
	done := make(chan struct{})
	go func() {
		tok.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return tok.Error()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("publishing to %s: timed out", topic)
	}
}

// Close disconnects from the broker.
func (m *MQTT) Close() {
	if m.client != nil {
		m.client.Disconnect(250)
	}
}

// clientID keeps broker client ids unique enough to avoid two instances
// kicking each other off, which MQTT brokers do silently.
func clientID(base string) string {
	if base == "" {
		base = "scootless"
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano()%100000)
}

// LogSink writes notifications to a logger. It is what runs when no broker is
// configured, so the watch machinery is fully usable before any messaging
// exists.
type LogSink struct{ Log *slog.Logger }

// Publish logs the notification.
func (l LogSink) Publish(ctx context.Context, n poll.Notification) error {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("watch fired",
		"watch", n.WatchID, "kind", string(n.Kind),
		"fence", n.Fence.Name, "count", n.Count, "vehicles", len(n.Vehicles))
	return nil
}
