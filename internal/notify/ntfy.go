package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deviationist/scootless/internal/poll"
	"github.com/deviationist/scootless/internal/store"
)

// Ntfy delivers notifications to an ntfy server, which is the hop that
// actually reaches a phone. MQTT carries the event around the house; this is
// what makes it buzz in a pocket.
type Ntfy struct {
	// Server is the base URL, e.g. https://ntfy.sh or a self-hosted one.
	Server string
	// Topic is the ntfy topic. On a public server the topic name is the only
	// thing standing between the notification and anyone who guesses it, so
	// it should be long and random rather than "scooters".
	Topic string
	// Token authenticates to a server that requires it. Optional.
	Token string
	// Priority is the ntfy priority for appearance alerts, 1-5. Zero uses a
	// high priority, since "a scooter just appeared" is time-critical and
	// pointless if it arrives quietly an hour later.
	Priority int

	HTTP *http.Client
}

// Publish sends a notification.
func (n *Ntfy) Publish(ctx context.Context, note poll.Notification) error {
	if n.Server == "" || n.Topic == "" {
		return fmt.Errorf("ntfy: server and topic are required")
	}
	p := PayloadOf(note)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(n.Server, "/")+"/"+n.Topic,
		bytes.NewBufferString(ntfyBody(p)))
	if err != nil {
		return err
	}
	req.Header.Set("Title", ntfyTitle(p))
	req.Header.Set("Tags", "scooter")
	req.Header.Set("Priority", strconv.Itoa(n.priority(p)))
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	// Tapping the notification opens the operator's own app on that exact
	// vehicle, which is the whole point: the alert is one tap from a ride
	// rather than an invitation to go hunting in three apps.
	if link := firstAppLink(p); link != "" {
		req.Header.Set("Click", link)
	}

	resp, err := n.client().Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// priority sends appearance alerts loudly and scarcity warnings quietly. One
// says "go now"; the other is planning information.
func (n *Ntfy) priority(p Payload) int {
	if p.Kind == string(store.KindScarcity) {
		return 3
	}
	if n.Priority > 0 {
		return n.Priority
	}
	return 4
}

func ntfyTitle(p Payload) string {
	if len(p.Vehicles) == 0 {
		return "Scooters running low"
	}
	v := p.Vehicles[0]
	if p.Kind == string(store.KindScarcity) {
		return fmt.Sprintf("%d left near %s", p.Count, p.Fence.Name)
	}
	return fmt.Sprintf("%s %d m away", v.Operator, v.DistanceM)
}

// ntfyBody lists what turned up, nearest first, in the shape you would want
// while putting your shoes on.
func ntfyBody(p Payload) string {
	if len(p.Vehicles) == 0 {
		return fmt.Sprintf("%d within %d m of %s", p.Count, p.Fence.RadiusM, p.Fence.Name)
	}
	var b strings.Builder
	for i, v := range p.Vehicles {
		if i >= 5 {
			fmt.Fprintf(&b, "and %d more", len(p.Vehicles)-i)
			break
		}
		fmt.Fprintf(&b, "%s · %d m %s · %.1f km range", v.Operator, v.DistanceM, v.Bearing, v.RangeKM)
		if v.BatteryPct != nil {
			fmt.Fprintf(&b, " · %.0f%%", *v.BatteryPct)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstAppLink(p Payload) string {
	for _, v := range p.Vehicles {
		if v.AppLink != "" {
			return v.AppLink
		}
	}
	return ""
}

func (n *Ntfy) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Multi fans a notification out to several sinks.
//
// A failing sink must not stop the others: if MQTT is down, the phone should
// still buzz, and vice versa. Errors are collected rather than short-circuited.
type Multi []poll.Sink

// Publish sends to every sink, returning whatever failed.
func (m Multi) Publish(ctx context.Context, n poll.Notification) error {
	var errs []error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Publish(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(errs)
}

// joinErrors combines sink failures without hiding any of them.
func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return errors.Join(errs...)
	}
}
