package geo

import (
	"math"
	"testing"
)

var (
	karlJohan = Point{Lat: 59.9139, Lon: 10.7522}
	osloS     = Point{Lat: 59.9107, Lon: 10.7522} // due south, same meridian
)

func TestDistanceMKnownPairs(t *testing.T) {
	tests := []struct {
		name   string
		a, b   Point
		wantM  float64
		tolerM float64
	}{
		{"identical", karlJohan, karlJohan, 0, 0.001},
		{"due south 0.0032 deg", karlJohan, osloS, 355.9, 1},
		// Oslo -> Trondheim, a distance where a wrong earth radius or a
		// degrees/radians slip would be obvious rather than subtle.
		{"oslo to trondheim", karlJohan, Point{Lat: 63.4305, Lon: 10.3951}, 391600, 1500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DistanceM(tc.a, tc.b)
			if math.Abs(got-tc.wantM) > tc.tolerM {
				t.Errorf("DistanceM = %.1f m, want %.1f +/- %.1f", got, tc.wantM, tc.tolerM)
			}
		})
	}
}

func TestDistanceMIsSymmetric(t *testing.T) {
	ab := DistanceM(karlJohan, osloS)
	ba := DistanceM(osloS, karlJohan)
	if math.Abs(ab-ba) > 1e-9 {
		t.Errorf("asymmetric: %.9f vs %.9f", ab, ba)
	}
}

func TestBearingCardinals(t *testing.T) {
	const d = 0.01
	tests := []struct {
		name string
		to   Point
		want string
	}{
		{"north", Point{karlJohan.Lat + d, karlJohan.Lon}, "N"},
		{"south", Point{karlJohan.Lat - d, karlJohan.Lon}, "S"},
		{"east", Point{karlJohan.Lat, karlJohan.Lon + d}, "E"},
		{"west", Point{karlJohan.Lat, karlJohan.Lon - d}, "W"},
		{"northeast", Point{karlJohan.Lat + d, karlJohan.Lon + d*2}, "NE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compass(BearingDeg(karlJohan, tc.to)); got != tc.want {
				t.Errorf("Compass = %q, want %q (bearing %.1f)",
					got, tc.want, BearingDeg(karlJohan, tc.to))
			}
		})
	}
}

// Compass must not panic or wrap incorrectly anywhere on the circle - the
// index arithmetic is the kind that fails only at the seam.
func TestCompassCoversFullCircle(t *testing.T) {
	for deg := 0.0; deg < 360; deg += 0.5 {
		if got := Compass(deg); got == "" {
			t.Fatalf("empty compass label at %.1f deg", deg)
		}
	}
	if got := Compass(359.9); got != "N" {
		t.Errorf("Compass(359.9) = %q, want N", got)
	}
	if got := Compass(0); got != "N" {
		t.Errorf("Compass(0) = %q, want N", got)
	}
}

func TestBoundingRadiusCoversEveryFence(t *testing.T) {
	centres := []Point{
		karlJohan,
		{Lat: 59.9160, Lon: 10.7600},
		{Lat: 59.9100, Lon: 10.7450},
	}
	radii := []float64{150, 200, 100}

	mid, r := BoundingRadiusM(centres, radii)
	for i, c := range centres {
		// Every point of every fence must lie inside the bounding circle,
		// or coalescing queries would silently drop vehicles.
		if reach := DistanceM(mid, c) + radii[i]; reach > r+1e-6 {
			t.Errorf("fence %d reaches %.1f m but bound is %.1f m", i, reach, r)
		}
	}
}

func TestBoundingRadiusEmpty(t *testing.T) {
	if _, r := BoundingRadiusM(nil, nil); r != 0 {
		t.Errorf("radius = %v, want 0", r)
	}
}
