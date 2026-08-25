package entur

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/deviationist/scootless/internal/geo"
)

var here = geo.Point{Lat: 59.9139, Lon: 10.7522}

// serveJSON stands in for the upstream API so the filtering rules can be
// tested without depending on what is parked in Oslo today.
func serveJSON(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("ET-Client-Name"); got == "" {
			t.Errorf("ET-Client-Name header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	c := New("scootless-test")
	c.Endpoint = srv.URL
	return c
}

func vehicleJSON(id string, lat, lon float64, form string, disabled, reserved bool,
	rangeM float64, fuel *float64, operatorID string) string {
	fuelPart := "null"
	if fuel != nil {
		fuelPart = fmt.Sprintf("%v", *fuel)
	}
	return fmt.Sprintf(`{"id":%q,"lat":%v,"lon":%v,"isReserved":%v,"isDisabled":%v,
      "currentRangeMeters":%v,"currentFuelPercent":%s,
      "vehicleType":{"formFactor":%q},
      "rentalUris":{"android":"a","ios":"i"},
      "system":{"operator":{"id":%q,"name":{"translation":[{"value":"X"}]}}}}`,
		id, lat, lon, reserved, disabled, rangeM, fuelPart, form, operatorID)
}

func wrap(vehicles ...string) string {
	return `{"data":{"vehicles":[` + strings.Join(vehicles, ",") + `]}}`
}

func TestVehiclesFiltersUnrentableAndNonScooters(t *testing.T) {
	fuel := 0.42
	c := serveJSON(t, wrap(
		vehicleJSON("ok", 59.9140, 10.7522, "SCOOTER_STANDING", false, false, 20000, &fuel, "YRY:Operator:Ryde"),
		vehicleJSON("disabled", 59.9140, 10.7522, "SCOOTER_STANDING", true, false, 20000, &fuel, "YRY:Operator:Ryde"),
		vehicleJSON("reserved", 59.9140, 10.7522, "SCOOTER_STANDING", false, true, 20000, &fuel, "YRY:Operator:Ryde"),
		// Voi really does return bicycles in the scooter feed.
		vehicleJSON("bike", 59.9140, 10.7522, "BICYCLE", false, false, 20000, &fuel, "YVO:Operator:voi"),
	))

	res, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 500})
	if err != nil {
		t.Fatal(err)
	}
	if res.Returned != 4 {
		t.Errorf("Returned = %d, want 4", res.Returned)
	}
	if res.Count() != 1 {
		t.Fatalf("kept %d vehicles, want 1: %+v", res.Count(), res.Vehicles)
	}
	if got := res.Vehicles[0].ID; got != "ok" {
		t.Errorf("kept %q, want \"ok\"", got)
	}
}

func TestVehiclesNormalisesFuelPercent(t *testing.T) {
	fuel := 0.35
	c := serveJSON(t, wrap(
		// Bolt and Voi report a 0-1 fraction...
		vehicleJSON("withfuel", 59.9140, 10.7522, "SCOOTER_STANDING", false, false, 20000, &fuel, "YBO:Operator:bolt"),
		// ...and Ryde reports nothing at all, which must stay distinct from 0.
		vehicleJSON("nofuel", 59.9141, 10.7522, "SCOOTER_STANDING", false, false, 30000, nil, "YRY:Operator:Ryde"),
	))
	res, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 500})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Vehicle{}
	for _, v := range res.Vehicles {
		byID[v.ID] = v
	}
	got, ok := byID["withfuel"]
	if !ok {
		t.Fatal("withfuel missing")
	}
	if got.FuelPct == nil || *got.FuelPct < 34.9 || *got.FuelPct > 35.1 {
		t.Errorf("FuelPct = %v, want ~35", got.FuelPct)
	}
	if byID["nofuel"].FuelPct != nil {
		t.Errorf("FuelPct = %v, want nil for an operator that omits it", byID["nofuel"].FuelPct)
	}
}

func TestVehiclesAppliesMinRange(t *testing.T) {
	c := serveJSON(t, wrap(
		vehicleJSON("flat", 59.9140, 10.7522, "SCOOTER_STANDING", false, false, 3000, nil, "YRY:Operator:Ryde"),
		vehicleJSON("full", 59.9141, 10.7522, "SCOOTER_STANDING", false, false, 30000, nil, "YRY:Operator:Ryde"),
	))
	res, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 500, MinRangeM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count() != 1 || res.Vehicles[0].ID != "full" {
		t.Errorf("got %+v, want only \"full\"", res.Vehicles)
	}
}

func TestVehiclesSortsByDistanceAndTrimsOutsideRadius(t *testing.T) {
	c := serveJSON(t, wrap(
		vehicleJSON("far", 59.9200, 10.7522, "SCOOTER_STANDING", false, false, 20000, nil, "YRY:Operator:Ryde"),
		vehicleJSON("near", 59.91395, 10.7522, "SCOOTER_STANDING", false, false, 20000, nil, "YRY:Operator:Ryde"),
		vehicleJSON("mid", 59.9145, 10.7522, "SCOOTER_STANDING", false, false, 20000, nil, "YRY:Operator:Ryde"),
	))
	res, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 100})
	if err != nil {
		t.Fatal(err)
	}
	// "far" is ~680 m away and must be trimmed even though the fake server
	// ignored the radius - real coalesced queries over-fetch on purpose.
	if res.Count() != 2 {
		t.Fatalf("kept %d, want 2: %+v", res.Count(), res.Vehicles)
	}
	if res.Vehicles[0].ID != "near" || res.Vehicles[1].ID != "mid" {
		t.Errorf("order = %q,%q; want near,mid", res.Vehicles[0].ID, res.Vehicles[1].ID)
	}
}

func TestVehiclesDetectsTruncation(t *testing.T) {
	parts := make([]string, 0, MaxResults)
	for i := 0; i < MaxResults; i++ {
		parts = append(parts, vehicleJSON(fmt.Sprintf("v%d", i), 59.9140, 10.7522,
			"SCOOTER_STANDING", false, false, 20000, nil, "YRY:Operator:Ryde"))
	}
	c := serveJSON(t, wrap(parts...))
	res, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true when the API returns exactly its cap")
	}
}

// A GraphQL failure arrives with HTTP 200 and an errors array. Missing it is
// how a broken query becomes a confident "no scooters nearby".
func TestVehiclesSurfacesGraphQLErrors(t *testing.T) {
	c := serveJSON(t, `{"errors":[{"message":"Validation error: bad field"}],"data":null}`)
	_, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 500})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "bad field") {
		t.Errorf("error = %v, want it to carry the upstream message", err)
	}
}

func TestVehiclesRejectsUnknownOperatorBeforeCalling(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	c := New("scootless-test")
	c.Endpoint = srv.URL

	_, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 100,
		OperatorKeys: []string{"ryde", "tier"}})
	if err == nil {
		t.Fatal("want an error for an unknown operator")
	}
	// Upstream would answer an unknown ID with an empty list and no error,
	// so the request must not be made at all.
	if called {
		t.Error("request was sent despite an unknown operator key")
	}
}

func TestOperatorIDsDefaultsToEveryOperator(t *testing.T) {
	ids, err := OperatorIDs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(Operators()) {
		t.Errorf("got %d ids, want %d", len(ids), len(Operators()))
	}
}

// Capitalisation is load-bearing: the lowercase form returns an empty list.
func TestRydeOperatorIDCapitalisation(t *testing.T) {
	o, ok := Lookup("ryde")
	if !ok {
		t.Fatal("ryde not registered")
	}
	if o.ID != "YRY:Operator:Ryde" {
		t.Errorf("ID = %q, want YRY:Operator:Ryde", o.ID)
	}
}

func TestVehiclesRejectsNonPositiveRadius(t *testing.T) {
	c := New("scootless-test")
	if _, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 0}); err == nil {
		t.Error("want an error for a zero radius")
	}
}

// TestLiveOslo runs against the real API. Skipped unless SCOOTLESS_LIVE=1, so
// the suite stays offline and deterministic by default.
func TestLiveOslo(t *testing.T) {
	if os.Getenv("SCOOTLESS_LIVE") != "1" {
		t.Skip("set SCOOTLESS_LIVE=1 to exercise the real API")
	}
	c := New("scootless-test")
	res, err := c.Vehicles(context.Background(), Query{At: here, RadiusM: 300})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("returned=%d kept=%d truncated=%v", res.Returned, res.Count(), res.Truncated)
	for _, v := range res.Vehicles {
		if v.FormFactor != ScooterStanding {
			t.Errorf("%s: form factor %q survived filtering", v.ID, v.FormFactor)
		}
		if !v.Rentable() {
			t.Errorf("%s: unrentable vehicle survived filtering", v.ID)
		}
		if v.DistanceM > 300 {
			t.Errorf("%s: %.0f m is outside the requested radius", v.ID, v.DistanceM)
		}
	}
	if res.Count() > 0 {
		v := res.Vehicles[0]
		t.Logf("nearest: %.0f m %s %s range=%.1f km", v.DistanceM, v.Compass(),
			v.Operator, float64(v.RangeM)/1000)
	}
	if _, err := json.Marshal(res.Vehicles); err != nil {
		t.Errorf("vehicles are not JSON-serialisable: %v", err)
	}
}
