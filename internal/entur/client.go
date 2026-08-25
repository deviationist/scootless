package entur

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

// Client queries Entur's mobility API.
type Client struct {
	Endpoint   string
	ClientName string
	HTTP       *http.Client
}

// New returns a client identifying itself as clientName, which Entur asks for
// in the ET-Client-Name header.
func New(clientName string) *Client {
	if clientName == "" {
		clientName = "scootless"
	}
	return &Client{
		Endpoint:   DefaultEndpoint,
		ClientName: clientName,
		HTTP:       &http.Client{Timeout: 25 * time.Second},
	}
}

// Query describes one question about one place.
type Query struct {
	At      geo.Point
	RadiusM int

	// OperatorKeys restricts the search. Empty means every supported operator.
	OperatorKeys []string

	// MinRangeM drops vehicles with less remaining range than this.
	MinRangeM int

	// FormFactors restricts vehicle classes. Empty means standing scooters
	// only, because Voi returns bicycles in the same feed.
	FormFactors []FormFactor

	// IncludeUnrentable keeps vehicles the app would refuse to rent. Off by
	// default; you cannot ride a disabled scooter.
	IncludeUnrentable bool

	// Limit caps the upstream request. Zero means MaxResults.
	Limit int
}

// Result is what came back, already filtered and sorted by walking distance.
type Result struct {
	Vehicles  []Vehicle
	Returned  int  // how many the API sent, before filtering
	Truncated bool // the API hit its cap, so the true count is unknown
	FetchedAt time.Time
}

// Count is the number of vehicles that survived filtering.
func (r *Result) Count() int { return len(r.Vehicles) }

const vehiclesQuery = `
query ($lat: Float!, $lon: Float!, $range: Int!, $operators: [String], $count: Int!) {
  vehicles(lat: $lat, lon: $lon, range: $range, operators: $operators, count: $count) {
    id
    lat
    lon
    isReserved
    isDisabled
    currentRangeMeters
    currentFuelPercent
    vehicleType { formFactor }
    rentalUris { android ios }
    system { operator { id name { translation { value } } } }
  }
}`

type gqlVehicle struct {
	ID                 string   `json:"id"`
	Lat                float64  `json:"lat"`
	Lon                float64  `json:"lon"`
	IsReserved         bool     `json:"isReserved"`
	IsDisabled         bool     `json:"isDisabled"`
	CurrentRangeMeters *float64 `json:"currentRangeMeters"`
	CurrentFuelPercent *float64 `json:"currentFuelPercent"`
	VehicleType        *struct {
		FormFactor string `json:"formFactor"`
	} `json:"vehicleType"`
	RentalUris *struct {
		Android string `json:"android"`
		IOS     string `json:"ios"`
	} `json:"rentalUris"`
	System *struct {
		Operator *struct {
			ID   string `json:"id"`
			Name *struct {
				Translation []struct {
					Value string `json:"value"`
				} `json:"translation"`
			} `json:"name"`
		} `json:"operator"`
	} `json:"system"`
}

type gqlResponse struct {
	Data struct {
		Vehicles []gqlVehicle `json:"vehicles"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// OperatorIDs turns short keys into upstream IDs, rejecting anything unknown.
//
// This validation is not politeness. An unrecognised operator ID makes the API
// return an empty list rather than an error, so a typo is indistinguishable
// from "there are no scooters here" - the single most misleading failure this
// API offers.
func OperatorIDs(keys []string) ([]string, error) {
	if len(keys) == 0 {
		ids := make([]string, 0, len(operators))
		for _, o := range operators {
			ids = append(ids, o.ID)
		}
		return ids, nil
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		o, ok := Lookup(k)
		if !ok {
			return nil, fmt.Errorf("unknown operator %q", k)
		}
		ids = append(ids, o.ID)
	}
	return ids, nil
}

// Vehicles runs one query and returns the vehicles that survive filtering,
// nearest first.
func (c *Client) Vehicles(ctx context.Context, q Query) (*Result, error) {
	if q.RadiusM <= 0 {
		return nil, fmt.Errorf("radius must be positive, got %d", q.RadiusM)
	}
	ids, err := OperatorIDs(q.OperatorKeys)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxResults {
		limit = MaxResults
	}

	body, err := json.Marshal(map[string]any{
		"query": vehiclesQuery,
		"variables": map[string]any{
			"lat": q.At.Lat, "lon": q.At.Lon, "range": q.RadiusM,
			"operators": ids, "count": limit,
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(),
		bytes.NewReader(body))
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
		return nil, fmt.Errorf("mobility API: unexpected status %s", resp.Status)
	}

	var out gqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mobility API: decoding response: %w", err)
	}
	// GraphQL reports failures in the body with a 200 status, so this check is
	// the only thing standing between a bad query and a silent empty result.
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("mobility API: %s", out.Errors[0].Message)
	}

	raw := out.Data.Vehicles
	res := &Result{
		Returned:  len(raw),
		Truncated: len(raw) >= limit && limit == MaxResults,
		FetchedAt: time.Now(),
	}
	for _, rv := range raw {
		v, keep := convert(rv, q)
		if keep {
			res.Vehicles = append(res.Vehicles, v)
		}
	}
	sort.Slice(res.Vehicles, func(i, j int) bool {
		return res.Vehicles[i].DistanceM < res.Vehicles[j].DistanceM
	})
	return res, nil
}

// convert maps one upstream vehicle onto ours, applying every filter in Query.
func convert(rv gqlVehicle, q Query) (Vehicle, bool) {
	if !q.IncludeUnrentable && (rv.IsDisabled || rv.IsReserved) {
		return Vehicle{}, false
	}

	form := FormFactor("")
	if rv.VehicleType != nil {
		form = FormFactor(rv.VehicleType.FormFactor)
	}
	if !allowedForm(form, q.FormFactors) {
		return Vehicle{}, false
	}

	rangeM := 0
	if rv.CurrentRangeMeters != nil {
		rangeM = int(*rv.CurrentRangeMeters)
	}
	if rangeM < q.MinRangeM {
		return Vehicle{}, false
	}

	at := geo.Point{Lat: rv.Lat, Lon: rv.Lon}
	dist := geo.DistanceM(q.At, at)
	// The API filters by radius server-side; this is belt and braces, and it
	// also trims the over-fetch when several fences share one query. A zero
	// radius means no area limit at all, which is how look-ups by id arrive.
	if q.RadiusM > 0 && dist > float64(q.RadiusM) {
		return Vehicle{}, false
	}

	// currentFuelPercent is a 0-1 fraction upstream. Normalise to 0-100 and
	// keep nil distinguishable from zero: Ryde reports nothing at all, which
	// is not the same as a flat battery.
	var pct *float64
	if rv.CurrentFuelPercent != nil {
		p := *rv.CurrentFuelPercent * 100
		pct = &p
	}

	v := Vehicle{
		ID:         rv.ID,
		At:         at,
		DistanceM:  dist,
		BearingDeg: geo.BearingDeg(q.At, at),
		FormFactor: form,
		RangeM:     rangeM,
		FuelPct:    pct,
		Disabled:   rv.IsDisabled,
		Reserved:   rv.IsReserved,
	}
	if rv.RentalUris != nil {
		v.AppLinkIOS = rv.RentalUris.IOS
		v.AppLinkAndroid = rv.RentalUris.Android
	}
	if rv.System != nil && rv.System.Operator != nil {
		op := rv.System.Operator
		if o, ok := byID(op.ID); ok {
			v.OperatorKey, v.Operator = o.Key, o.Name
		} else if op.Name != nil && len(op.Name.Translation) > 0 {
			v.Operator = op.Name.Translation[0].Value
		}
	}
	return v, true
}

// allowedForm defaults to standing scooters only. Voi mixes bicycles into the
// same feed, and a city bike is not a scooter however the API files it.
func allowedForm(f FormFactor, allowed []FormFactor) bool {
	if len(allowed) == 0 {
		return f == ScooterStanding
	}
	for _, a := range allowed {
		if a == f {
			return true
		}
	}
	return false
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
		return &http.Client{Timeout: 25 * time.Second}
	}
	return c.HTTP
}

const byIDQuery = `
query ($ids: [String!]) {
  vehicles(ids: $ids) {
    id
    lat
    lon
    isReserved
    isDisabled
    currentRangeMeters
    currentFuelPercent
    vehicleType { formFactor }
    rentalUris { android ios }
    system { operator { id name { translation { value } } } }
  }
}`

// ByID looks vehicles up by identifier, wherever they are.
//
// This is not a radius query, so it is not subject to the nearest-N selection
// that count imposes: a vehicle three kilometres away is returned just as
// readily as one outside the door. An id that is absent from the result is
// absent from the feed entirely - which, for a rentable vehicle, most often
// means somebody is riding it, since an active rental removes it from the feed
// rather than flagging it.
//
// Filters in q that concern a search area are ignored; only MinRangeM,
// FormFactors and IncludeUnrentable apply. Distances and bearings are relative
// to q.At, which may be the zero value if the caller does not care.
func (c *Client) ByID(ctx context.Context, ids []string, q Query) (map[string]Vehicle, error) {
	out := make(map[string]Vehicle, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string]any{
		"query":     byIDQuery,
		"variables": map[string]any{"ids": ids},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(),
		bytes.NewReader(body))
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
		return nil, fmt.Errorf("mobility API: unexpected status %s", resp.Status)
	}
	var out2 gqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&out2); err != nil {
		return nil, fmt.Errorf("mobility API: decoding response: %w", err)
	}
	if len(out2.Errors) > 0 {
		return nil, fmt.Errorf("mobility API: %s", out2.Errors[0].Message)
	}

	// A radius of zero would make convert drop everything, so look-ups by id
	// are not distance-filtered at all.
	lookup := q
	lookup.RadiusM = 0
	for _, rv := range out2.Data.Vehicles {
		v, keep := convert(rv, lookup)
		if !keep {
			continue
		}
		out[v.ID] = v
	}
	return out, nil
}
