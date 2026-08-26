// Package board ranks nearby scooters under a personal preference and picks the
// best one to take — the recommendation a display shows when you leave the
// house.
package board

import (
	"sort"

	"github.com/deviationist/scootless/internal/entur"
)

// Prefs captures how attractive each option is, so the board can recommend one
// rather than just listing what exists.
//
// The default reflects "Ryde first, Voi as the alternative, Bolt last, and a
// low battery makes any of them less attractive." It is a soft preference, not
// an absolute: a much closer Voi beats a distant Ryde, which is the whole point
// of scoring rather than filtering.
type Prefs struct {
	// OperatorBonus is added to an operator's score. The gap between two
	// operators is, in effect, how many metres of extra walk you'll accept to
	// stay on your preferred brand (because distance costs 1 point/metre).
	OperatorBonus map[string]float64

	// Battery is treated as a penalty, not a reward: at or above ComfortKM
	// there is no penalty, and it grows to LowPenalty as range falls to FloorKM.
	// A penalty (rather than a mild reward) is what lets a nearly-flat scooter
	// actually lose to a healthy one — "low battery makes it less attractive"
	// only means something if the penalty can outweigh the operator preference.
	ComfortKM  float64
	LowPenalty float64

	// FloorKM excludes scooters below this remaining range outright — below a
	// point, low battery isn't "less attractive", it's useless.
	FloorKM float64
}

// DefaultPrefs is the preference described above.
func DefaultPrefs() Prefs {
	return Prefs{
		OperatorBonus: map[string]float64{
			"ryde": 300, // preferred
			"voi":  150, // alternative — wins if ~150 m+ closer than Ryde
			"bolt": 50,  // last resort
			"dott": 0,
		},
		ComfortKM:  10,  // ≥10 km range: battery is a non-issue
		LowPenalty: 250, // at the floor, a full 250-point hit — enough to lose a brand
		FloorKM:    2,
	}
}

// Option is one scooter, scored.
type Option struct {
	Operator    string   `json:"operator"`
	OperatorKey string   `json:"operator_key"`
	Number      string   `json:"number"`
	DistanceM   int      `json:"distance_m"`
	Bearing     string   `json:"bearing"`
	RangeKM     float64  `json:"range_km"`
	BatteryPct  *float64 `json:"battery_pct"`
	AppLink     string   `json:"app_link,omitempty"`
	Score       float64  `json:"score"`
}

// OperatorSummary is the at-a-glance count and best pick per operator.
type OperatorSummary struct {
	Count       int      `json:"count"`
	NearestM    *int     `json:"nearest_m"`
	BestRangeKM *float64 `json:"best_range_km"`
}

// Board is the ranked scooter view: the best one to take, the alternatives,
// and a per-operator summary.
type Board struct {
	Recommendation *Option                    `json:"recommendation"`
	Options        []Option                   `json:"options"`
	ByOperator     map[string]OperatorSummary `json:"by_operator"`
}

// score rates one vehicle under the preference. Higher is better.
func (p Prefs) score(v entur.Vehicle) (float64, bool) {
	rangeKM := float64(v.RangeM) / 1000
	if p.FloorKM > 0 && rangeKM < p.FloorKM {
		return 0, false // below the floor: not an option at all
	}
	s := p.OperatorBonus[v.OperatorKey]
	s -= v.DistanceM // 1 point per metre of walk
	s -= lowBatteryPenalty(rangeKM, p.ComfortKM, p.FloorKM, p.LowPenalty)
	return s, true
}

// lowBatteryPenalty is 0 at/above comfort and rises linearly to maxPenalty as
// range falls to the floor.
func lowBatteryPenalty(rangeKM, comfortKM, floorKM, maxPenalty float64) float64 {
	if rangeKM >= comfortKM || comfortKM <= floorKM {
		return 0
	}
	frac := (comfortKM - rangeKM) / (comfortKM - floorKM)
	if frac > 1 {
		frac = 1
	}
	return maxPenalty * frac
}

// Assemble scores the vehicles, ranks them, and summarises per operator. It
// does not fetch anything — the caller supplies the vehicles, which keeps this
// package pure and testable.
func Assemble(vehicles []entur.Vehicle, prefs Prefs, limit int) Board {
	b := Board{ByOperator: map[string]OperatorSummary{}}

	for _, v := range vehicles {
		key := v.OperatorKey
		sum := b.ByOperator[key]
		sum.Count++
		d := int(v.DistanceM + 0.5)
		if sum.NearestM == nil || d < *sum.NearestM {
			sum.NearestM = &d
		}
		rk := float64(v.RangeM) / 1000
		if sum.BestRangeKM == nil || rk > *sum.BestRangeKM {
			sum.BestRangeKM = &rk
		}
		b.ByOperator[key] = sum

		s, ok := prefs.score(v)
		if !ok {
			continue
		}
		b.Options = append(b.Options, Option{
			Operator:    v.Operator,
			OperatorKey: v.OperatorKey,
			Number:      number(v.ID),
			DistanceM:   d,
			Bearing:     v.Compass(),
			RangeKM:     round1(rk),
			BatteryPct:  v.FuelPct,
			AppLink:     v.AppLinkIOS,
			Score:       round1(s),
		})
	}

	sort.SliceStable(b.Options, func(i, j int) bool { return b.Options[i].Score > b.Options[j].Score })
	if len(b.Options) > 0 {
		best := b.Options[0]
		b.Recommendation = &best
	}
	if limit > 0 && len(b.Options) > limit {
		b.Options = b.Options[:limit]
	}
	return b
}

// number extracts the visible scooter number from an Entur id (…:eaNNNNNN-…),
// falling back to the raw id for operators without that scheme.
func number(id string) string {
	last := id
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' {
			last = id[i+1:]
			break
		}
	}
	if len(last) >= 8 && last[:2] == "ea" {
		return last[2:8]
	}
	return last
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
