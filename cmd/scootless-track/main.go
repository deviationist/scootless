// Command scootless-track follows named vehicles and reports where they go.
//
// It exists to answer one question honestly: when a scooter leaves, was it
// ridden or was it moved? An active rental removes a vehicle from the feed
// rather than flagging it, so a vehicle that cannot be looked up is in use,
// and one that reappears somewhere else has been parked there.
//
// It tracks only the vehicles it is given. It is not, and should not become, a
// way to log a city.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/deviationist/scootless/internal/config"
	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/poll"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scootless-track:", err)
		os.Exit(1)
	}
}

type state struct {
	seen     bool
	at       geo.Point
	since    time.Time
	start    geo.Point
	hasStart bool
	goneAt   time.Time
}

func run() error {
	var (
		dir     = flag.String("config", ".", "directory to read .env from")
		every   = flag.Duration("every", poll.DefaultInterval, "how often to look")
		until   = flag.Duration("for", 2*time.Hour, "give up after this long")
		jsonOut = flag.Bool("json", false, "emit one JSON object per event")
	)
	flag.Parse()

	ids := flag.Args()
	if len(ids) == 0 {
		return fmt.Errorf("give one or more vehicle ids to follow\n" +
			"  e.g. scootless-track YRY:Vehicle:ea3...")
	}
	cfg, err := config.Load(*dir)
	if err != nil {
		return err
	}
	client := entur.New(cfg.ClientName)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	deadline := time.Now().Add(*until)

	states := map[string]*state{}
	for _, id := range ids {
		states[id] = &state{}
	}

	emit := func(id, event string, s *state, extra map[string]any) {
		if *jsonOut {
			rec := map[string]any{
				"at": time.Now().UTC().Format(time.RFC3339), "vehicle_id": id, "event": event,
			}
			for k, v := range extra {
				rec[k] = v
			}
			b, _ := json.Marshal(rec)
			fmt.Println(string(b))
			return
		}
		var parts []string
		for k, v := range extra {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		fmt.Printf("%s  %-14s %s  %s\n", time.Now().Format("15:04:05"),
			event, short(id), strings.Join(parts, " "))
	}

	fmt.Fprintf(os.Stderr, "following %d vehicle(s), every %s, for up to %s\n",
		len(ids), *every, *until)

	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		found, err := client.ByID(ctx, ids, entur.Query{})
		if err != nil {
			fmt.Fprintln(os.Stderr, "lookup failed:", err)
		} else {
			for _, id := range ids {
				s := states[id]
				v, present := found[id]
				switch {
				case present && !s.seen:
					if !s.hasStart {
						s.start, s.hasStart = v.At, true
						emit(id, "parked", s, map[string]any{
							"lat": v.At.Lat, "lon": v.At.Lon, "range_km": float64(v.RangeM) / 1000})
					} else {
						// Back after an absence: this is the end of a trip.
						d := geo.DistanceM(s.start, v.At)
						extra := map[string]any{
							"lat": v.At.Lat, "lon": v.At.Lon,
							"moved_m":    int(d + 0.5),
							"bearing":    geo.Compass(geo.BearingDeg(s.start, v.At)),
							"gone_for_s": int(time.Since(s.goneAt).Seconds()),
						}
						emit(id, "reappeared", s, extra)
						s.start = v.At
					}
					s.seen, s.at, s.since = true, v.At, time.Now()
				case present && s.seen:
					if moved := geo.DistanceM(s.at, v.At); moved > 15 {
						emit(id, "moved", s, map[string]any{
							"lat": v.At.Lat, "lon": v.At.Lon, "by_m": int(moved + 0.5)})
						s.at = v.At
					}
				case !present && s.seen:
					s.seen, s.goneAt = false, time.Now()
					emit(id, "vanished", s, map[string]any{
						"note": "in use, or withdrawn"})
				}
			}
		}

		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "time limit reached")
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "stopped")
			return nil
		case <-t.C:
		}
	}
}

func short(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 && len(id) > i+9 {
		return id[:i+9] + "…"
	}
	return id
}
