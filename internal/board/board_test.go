package board

import (
	"testing"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
)

func veh(op string, distM, rangeM int) entur.Vehicle {
	return entur.Vehicle{
		ID: "YRY:Vehicle:ea377489-x", Operator: op, OperatorKey: op,
		At: geo.Point{Lat: 59.9, Lon: 10.7}, DistanceM: float64(distM), RangeM: rangeM,
	}
}

// A nearby Ryde with decent battery should win outright.
func TestNearRydeIsRecommended(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 100, 26000),
		veh("voi", 50, 30000),
	}, DefaultPrefs(), 10)
	if b.Recommendation == nil || b.Recommendation.OperatorKey != "ryde" {
		t.Fatalf("recommendation = %+v, want a Ryde", b.Recommendation)
	}
}

// A moderately-distant Ryde still wins — the subscription is strong.
func TestModeratelyFarRydeStillWins(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 400, 26000), // 400 m is not "very far"
		veh("voi", 50, 30000),
	}, DefaultPrefs(), 10)
	if b.Recommendation.OperatorKey != "ryde" {
		t.Errorf("recommendation = %s, want ryde (400 m is not far enough to switch)", b.Recommendation.OperatorKey)
	}
}

// Only a *very* distant Ryde loses to a close Voi.
func TestVeryFarRydeLosesToCloseVoi(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 900, 26000), // a very long walk
		veh("voi", 50, 30000),
	}, DefaultPrefs(), 10)
	if b.Recommendation.OperatorKey != "voi" {
		t.Errorf("recommendation = %s, want voi (Ryde is a 900 m walk)", b.Recommendation.OperatorKey)
	}
}

// A very-low-battery Ryde is dropped, so a healthy Voi is recommended instead.
func TestVeryLowBatteryRydeFallsThroughToVoi(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 120, 2500), // close, but below the 4 km floor — nearly flat
		veh("voi", 140, 35000), // slightly further, full
	}, DefaultPrefs(), 10)
	if b.Recommendation == nil || b.Recommendation.OperatorKey != "voi" {
		t.Errorf("recommendation = %+v, want voi (the only Ryde is nearly flat)", b.Recommendation)
	}
	// but the flat Ryde still shows in the count — it exists, just isn't offered
	if b.ByOperator["ryde"].Count != 1 {
		t.Errorf("ryde count = %d, want 1", b.ByOperator["ryde"].Count)
	}
}

// A healthy but further Ryde still beats Voi when a nearer Ryde is flat — the
// subscription wins over Voi as long as *any* usable Ryde is around.
func TestHealthyRydeBeatsVoiEvenIfAnotherRydeIsFlat(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 60, 2500),   // nearest, but flat → dropped
		veh("ryde", 250, 30000), // usable Ryde, a bit further
		veh("voi", 80, 40000),   // close, full
	}, DefaultPrefs(), 10)
	if b.Recommendation.OperatorKey != "ryde" || b.Recommendation.DistanceM != 250 {
		t.Errorf("recommendation = %+v, want the 250 m Ryde", b.Recommendation)
	}
}

// Below the floor a scooter is not an option at all.
func TestBelowFloorIsExcluded(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 50, 1000), // 1 km range, below the 2 km floor
	}, DefaultPrefs(), 10)
	if b.Recommendation != nil {
		t.Errorf("recommended a below-floor scooter: %+v", b.Recommendation)
	}
	if len(b.Options) != 0 {
		t.Errorf("options = %+v, want none", b.Options)
	}
	// but it still shows in the per-operator count (it exists, just unusable)
	if b.ByOperator["ryde"].Count != 1 {
		t.Errorf("ryde count = %d, want 1", b.ByOperator["ryde"].Count)
	}
}

// Bolt is last: it only wins when nothing better is near.
func TestBoltIsLastResort(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("bolt", 40, 40000),
		veh("ryde", 120, 30000),
	}, DefaultPrefs(), 10)
	// ryde 120m: 300-120+60 = 240 ; bolt 40m: 50-40+60 = 70 -> ryde wins
	if b.Recommendation.OperatorKey != "ryde" {
		t.Errorf("recommendation = %s, want ryde over a closer Bolt", b.Recommendation.OperatorKey)
	}
	// but a lone Bolt is still recommended when it's all there is
	b = Assemble([]entur.Vehicle{veh("bolt", 40, 40000)}, DefaultPrefs(), 10)
	if b.Recommendation == nil || b.Recommendation.OperatorKey != "bolt" {
		t.Errorf("a lone Bolt should still be recommended, got %+v", b.Recommendation)
	}
}

func TestOptionsAreRankedByScore(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("bolt", 300, 20000),
		veh("ryde", 90, 26000),
		veh("voi", 120, 30000),
	}, DefaultPrefs(), 10)
	if len(b.Options) != 3 {
		t.Fatalf("options = %d, want 3", len(b.Options))
	}
	for i := 1; i < len(b.Options); i++ {
		if b.Options[i-1].Score < b.Options[i].Score {
			t.Errorf("options not sorted by score: %+v", b.Options)
		}
	}
	if b.Options[0].OperatorKey != "ryde" {
		t.Errorf("top option = %s, want ryde", b.Options[0].OperatorKey)
	}
}

func TestPerOperatorSummary(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 100, 26000),
		veh("ryde", 60, 30000),
		veh("voi", 200, 15000),
	}, DefaultPrefs(), 10)
	r := b.ByOperator["ryde"]
	if r.Count != 2 || r.NearestM == nil || *r.NearestM != 60 {
		t.Errorf("ryde summary = %+v", r)
	}
	if r.BestRangeKM == nil || *r.BestRangeKM != 30 {
		t.Errorf("ryde best range = %v, want 30", r.BestRangeKM)
	}
}

func TestNumberExtraction(t *testing.T) {
	if n := number("YRY:Vehicle:ea377489-65e1-3f09"); n != "377489" {
		t.Errorf("number = %q, want 377489", n)
	}
	if n := number("YVO:Vehicle:randomuuid"); n != "randomuuid" {
		t.Errorf("number = %q, want the raw id for non-ea", n)
	}
}

func TestLimitCapsOptionsButNotRecommendation(t *testing.T) {
	b := Assemble([]entur.Vehicle{
		veh("ryde", 90, 26000), veh("voi", 120, 30000), veh("bolt", 300, 20000),
	}, DefaultPrefs(), 1)
	if len(b.Options) != 1 {
		t.Errorf("options = %d, want 1 after limit", len(b.Options))
	}
	if b.Recommendation == nil {
		t.Error("recommendation dropped by the limit")
	}
}
