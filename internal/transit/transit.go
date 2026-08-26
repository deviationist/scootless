// Package transit fetches public-transport departures from Entur's national
// JourneyPlanner API — the same aggregator, and the same courtesy ET-Client-Name
// header, as the scooter feed. No API key.
//
// It exists so scootless can answer the whole leaving-the-house question — "is
// there a scooter, and when's the next bus" — from one backend.
package transit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/deviationist/scootless/internal/geo"
)

// DefaultEndpoint is Entur's public JourneyPlanner v3 service.
const DefaultEndpoint = "https://api.entur.io/journey-planner/v3/graphql"

// Client queries departures.
type Client struct {
	Endpoint   string
	ClientName string
	HTTP       *http.Client
	Now        func() time.Time
}

// New returns a client identifying itself as clientName.
func New(clientName string) *Client {
	if clientName == "" {
		clientName = "scootless"
	}
	return &Client{
		Endpoint:   DefaultEndpoint,
		ClientName: clientName,
		HTTP:       &http.Client{Timeout: 15 * time.Second},
	}
}

// Departure is one upcoming service call at a nearby stop.
type Departure struct {
	Stop      string    `json:"stop"`
	StopID    string    `json:"stop_id"`
	DistanceM int       `json:"dist_m"`
	Mode      string    `json:"mode"` // bus, tram, metro, rail, water, …
	Line      string    `json:"line"` // public-facing line code, e.g. "21"
	Dest      string    `json:"dest"`
	At        time.Time `json:"at"`
	InMinutes int       `json:"in_min"`
	Realtime  bool      `json:"realtime"`
}

// Query describes what to fetch.
type Query struct {
	At       geo.Point
	RadiusM  int           // how far to look for stops (default 500)
	MaxStops int           // cap the number of stops (default 8)
	PerStop  int           // departures per stop (default 4)
	Modes    []string      // restrict to these transport modes; empty = all
	Lines    []string      // restrict to these public line codes; empty = all
	Horizon  time.Duration // how far ahead to look (default 90m)
}

const departuresQuery = `
query ($lat: Float!, $lon: Float!, $radius: Float!, $stops: Int!, $per: Int!, $range: Int!) {
  nearest(latitude: $lat, longitude: $lon, maximumDistance: $radius,
          filterByPlaceTypes: stopPlace, first: $stops) {
    edges { node {
      distance
      place {
        ... on StopPlace {
          name
          id
          estimatedCalls(numberOfDepartures: $per, timeRange: $range) {
            expectedDepartureTime
            realtime
            destinationDisplay { frontText }
            serviceJourney { line { publicCode transportMode } }
          }
        }
      }
    } }
  }
}`

type gqlResp struct {
	Data struct {
		Nearest struct {
			Edges []struct {
				Node struct {
					Distance float64 `json:"distance"`
					Place    struct {
						Name           string `json:"name"`
						ID             string `json:"id"`
						EstimatedCalls []struct {
							Expected    string `json:"expectedDepartureTime"`
							Realtime    bool   `json:"realtime"`
							Destination struct {
								FrontText string `json:"frontText"`
							} `json:"destinationDisplay"`
							ServiceJourney struct {
								Line struct {
									PublicCode    string `json:"publicCode"`
									TransportMode string `json:"transportMode"`
								} `json:"line"`
							} `json:"serviceJourney"`
						} `json:"estimatedCalls"`
					} `json:"place"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"nearest"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Departures returns the next departures from nearby stops, soonest first.
func (c *Client) Departures(ctx context.Context, q Query) ([]Departure, error) {
	if q.RadiusM <= 0 {
		q.RadiusM = 500
	}
	if q.MaxStops <= 0 {
		q.MaxStops = 8
	}
	if q.PerStop <= 0 {
		q.PerStop = 4
	}
	if q.Horizon <= 0 {
		q.Horizon = 90 * time.Minute
	}

	body, err := json.Marshal(map[string]any{
		"query": departuresQuery,
		"variables": map[string]any{
			"lat": q.At.Lat, "lon": q.At.Lon, "radius": float64(q.RadiusM),
			"stops": q.MaxStops, "per": q.PerStop,
			"range": int(q.Horizon.Seconds()),
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ET-Client-Name", c.clientName())

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("journey-planner: unexpected status %s", resp.Status)
	}
	var out gqlResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("journey-planner: decoding: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("journey-planner: %s", out.Errors[0].Message)
	}

	modeOK := setOf(q.Modes)
	lineOK := setOf(q.Lines)
	now := c.now()

	var deps []Departure
	for _, e := range out.Data.Nearest.Edges {
		p := e.Node.Place
		for _, call := range p.EstimatedCalls {
			line := call.ServiceJourney.Line
			if len(modeOK) > 0 && !modeOK[line.TransportMode] {
				continue
			}
			if len(lineOK) > 0 && !lineOK[line.PublicCode] {
				continue
			}
			t, err := time.Parse(time.RFC3339, call.Expected)
			if err != nil {
				continue
			}
			deps = append(deps, Departure{
				Stop:      p.Name,
				StopID:    p.ID,
				DistanceM: int(e.Node.Distance + 0.5),
				Mode:      line.TransportMode,
				Line:      line.PublicCode,
				Dest:      call.Destination.FrontText,
				At:        t,
				InMinutes: int(t.Sub(now).Round(time.Minute).Minutes()),
				Realtime:  call.Realtime,
			})
		}
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].At.Before(deps[j].At) })
	return deps, nil
}

func setOf(v []string) map[string]bool {
	if len(v) == 0 {
		return nil
	}
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

func (c *Client) endpoint() string {
	if c.Endpoint == "" {
		return DefaultEndpoint
	}
	return c.Endpoint
}
func (c *Client) clientName() string {
	if c.ClientName == "" {
		return "scootless"
	}
	return c.ClientName
}
func (c *Client) http() *http.Client {
	if c.HTTP == nil {
		return &http.Client{Timeout: 15 * time.Second}
	}
	return c.HTTP
}
func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
