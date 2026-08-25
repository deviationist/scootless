// Package entur talks to Entur's national mobility API, which aggregates the
// GBFS feeds Norwegian micromobility operators are legally required to publish.
// No API key is involved; Entur asks only that callers identify themselves with
// an ET-Client-Name header.
//
// Several traits of the upstream API are non-obvious and cost real time to
// discover. They are encoded here so no caller has to rediscover them:
//
//   - Operator IDs are case-sensitive, and an unrecognised one returns an empty
//     list rather than an error. A typo therefore looks exactly like "no
//     scooters nearby". Keys are validated locally, before the request.
//   - Vehicles with isDisabled set appear in the feed but cannot be rented.
//   - Voi returns bicycles in the same feed as its scooters, so form factor
//     must be filtered or a city bike is reported as a scooter.
//   - currentFuelPercent is a 0-1 fraction, not a percentage, and Ryde leaves
//     it null on every vehicle. currentRangeMeters is the only battery signal
//     that every operator provides.
//   - The result set caps at 500. Exactly 500 back means it was truncated.
//   - Operator coverage is per-city: Dott runs in Trondheim but not Oslo.
//     Asking for an operator that is absent is not an error, just empty.
package entur

import "github.com/deviationist/scootless/internal/geo"

// MaxResults is the upstream cap. Getting exactly this many back means the
// list was cut short and the true count is unknown.
const MaxResults = 500

// DefaultEndpoint is Entur's public GraphQL service for mobility data.
const DefaultEndpoint = "https://api.entur.io/mobility/v2/graphql"

// FormFactor is the GBFS vehicle class.
type FormFactor string

const (
	ScooterStanding FormFactor = "SCOOTER_STANDING"
	Bicycle         FormFactor = "BICYCLE"
	Car             FormFactor = "CAR"
)

// Operator is one micromobility operator as Entur identifies it.
type Operator struct {
	Key  string // short name used in config and on the command line
	ID   string // Entur's identifier, case-sensitive
	Name string
}

// operators is the set scootless supports. The IDs were read from Entur's own
// operators query, not guessed; the capitalisation is load-bearing.
var operators = []Operator{
	{Key: "ryde", ID: "YRY:Operator:Ryde", Name: "Ryde"},
	{Key: "voi", ID: "YVO:Operator:voi", Name: "Voi"},
	{Key: "bolt", ID: "YBO:Operator:bolt", Name: "Bolt"},
	{Key: "dott", ID: "YDT:Operator:dott", Name: "Dott"},
}

// Operators returns every supported operator.
func Operators() []Operator {
	out := make([]Operator, len(operators))
	copy(out, operators)
	return out
}

// Lookup finds an operator by its short key.
func Lookup(key string) (Operator, bool) {
	for _, o := range operators {
		if o.Key == key {
			return o, true
		}
	}
	return Operator{}, false
}

// byID indexes operators for turning an upstream ID back into a key.
func byID(id string) (Operator, bool) {
	for _, o := range operators {
		if o.ID == id {
			return o, true
		}
	}
	return Operator{}, false
}

// Vehicle is one rentable thing, as scootless cares about it.
type Vehicle struct {
	ID          string
	At          geo.Point
	DistanceM   float64
	BearingDeg  float64
	OperatorKey string
	Operator    string
	FormFactor  FormFactor

	// RangeM is remaining range in metres - the only battery signal every
	// operator reports.
	RangeM int

	// FuelPct is 0-100, or nil when the operator does not report it. Ryde
	// never does.
	FuelPct *float64

	Disabled bool
	Reserved bool

	AppLinkIOS     string
	AppLinkAndroid string
}

// Compass is the eight-point direction you would walk to reach it.
func (v Vehicle) Compass() string { return geo.Compass(v.BearingDeg) }

// Rentable reports whether the app would actually let you take this one.
func (v Vehicle) Rentable() bool { return !v.Disabled && !v.Reserved }
