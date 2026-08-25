package notify

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/poll"
	"github.com/deviationist/scootless/internal/store"
)

// fakeToken satisfies mqtt.Token without a broker.
type fakeToken struct{ err error }

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }
func (t *fakeToken) Error() error                   { return t.err }

type fakeClient struct {
	topics   []string
	payloads [][]byte
	qos      []byte
	retained []bool
	err      error
}

func (c *fakeClient) Publish(topic string, qos byte, retained bool, payload any) mqtt.Token {
	c.topics = append(c.topics, topic)
	c.qos = append(c.qos, qos)
	c.retained = append(c.retained, retained)
	if b, ok := payload.([]byte); ok {
		c.payloads = append(c.payloads, b)
	}
	return &fakeToken{err: c.err}
}
func (c *fakeClient) IsConnected() bool       { return true }
func (c *fakeClient) Disconnect(quiesce uint) {}

func sampleNotification() poll.Notification {
	pct := 42.0
	return poll.Notification{
		WatchID: "w1",
		Kind:    store.KindAppearance,
		FiredAt: time.Unix(1787652000, 0).UTC(),
		Fence:   store.Fence{ID: "home", Name: "home", RadiusM: 150},
		Count:   1,
		Vehicles: []entur.Vehicle{{
			ID: "v1", Operator: "Ryde", DistanceM: 61.4, BearingDeg: 270,
			RangeM: 36200, FuelPct: &pct, AppLinkIOS: "ryde://x",
		}},
	}
}

func TestPublishTopicAndPayload(t *testing.T) {
	c := &fakeClient{}
	m := New(c, "scootless", nil)

	if err := m.Publish(context.Background(), sampleNotification()); err != nil {
		t.Fatal(err)
	}
	if len(c.topics) != 1 || c.topics[0] != "scootless/watch/w1/fired" {
		t.Fatalf("topics = %v", c.topics)
	}
	// A missed "your scooter is here" is the one message that must not be
	// dropped, so QoS 1; and it is worthless to a late subscriber, so not
	// retained.
	if c.qos[0] != 1 {
		t.Errorf("qos = %d, want 1", c.qos[0])
	}
	if c.retained[0] {
		t.Error("notification was published retained")
	}

	var p Payload
	if err := json.Unmarshal(c.payloads[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.WatchID != "w1" || p.Kind != "appearance" || p.Count != 1 {
		t.Errorf("payload = %+v", p)
	}
	if len(p.Vehicles) != 1 {
		t.Fatalf("vehicles = %+v", p.Vehicles)
	}
	v := p.Vehicles[0]
	if v.DistanceM != 61 {
		t.Errorf("DistanceM = %d, want 61", v.DistanceM)
	}
	if v.Bearing != "W" {
		t.Errorf("Bearing = %q, want W", v.Bearing)
	}
	if v.RangeKM != 36.2 {
		t.Errorf("RangeKM = %v, want 36.2", v.RangeKM)
	}
	if v.BatteryPct == nil || *v.BatteryPct != 42 {
		t.Errorf("BatteryPct = %v", v.BatteryPct)
	}
}

// A subscriber needs to know which watch fired, not where the person lives.
func TestPayloadDoesNotCarryCoordinates(t *testing.T) {
	c := &fakeClient{}
	m := New(c, "scootless", nil)
	n := sampleNotification()
	n.Fence.At.Lat = 59.9139
	n.Fence.At.Lon = 10.7522

	if err := m.Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	body := string(c.payloads[0])
	for _, needle := range []string{"59.9139", "10.7522", "\"lat\"", "\"lon\""} {
		if contains(body, needle) {
			t.Errorf("payload leaks %q: %s", needle, body)
		}
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(hay); i++ {
				if hay[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

func TestPublishSurfacesBrokerErrors(t *testing.T) {
	c := &fakeClient{err: errors.New("not connected")}
	m := New(c, "scootless", nil)
	if err := m.Publish(context.Background(), sampleNotification()); err == nil {
		t.Error("want the broker error to surface")
	}
}

func TestSampleTopic(t *testing.T) {
	c := &fakeClient{}
	m := New(c, "scootless", nil)
	err := m.Sample(context.Background(), "home",
		map[string]int{"ryde": 0, "voi": 2}, time.Unix(1787652000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if c.topics[0] != "scootless/fence/home/sample" {
		t.Errorf("topic = %q", c.topics[0])
	}
}

func TestPrefixTrailingSlashIsNormalised(t *testing.T) {
	c := &fakeClient{}
	m := New(c, "home/scootless/", nil)
	m.Publish(context.Background(), sampleNotification())
	if c.topics[0] != "home/scootless/watch/w1/fired" {
		t.Errorf("topic = %q", c.topics[0])
	}
}

func TestLogSinkNeverFails(t *testing.T) {
	if err := (LogSink{}).Publish(context.Background(), sampleNotification()); err != nil {
		t.Errorf("LogSink returned %v", err)
	}
}

func TestDialRequiresABroker(t *testing.T) {
	if _, err := Dial(context.Background(), Options{}); err == nil {
		t.Error("want an error with no broker configured")
	}
}
