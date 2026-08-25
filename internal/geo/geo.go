// Package geo holds the small amount of spherical geometry scootless needs:
// how far away a vehicle is, and which way you would walk to reach it.
package geo

import "math"

// earthRadiusM is the IUGG mean radius. At the distances that matter here —
// a few hundred metres — the choice of radius is far below the noise floor of
// the coordinates the operators publish.
const earthRadiusM = 6371008.8

// Point is a WGS84 coordinate in decimal degrees.
type Point struct {
	Lat float64
	Lon float64
}

// DistanceM returns the great-circle distance between two points, in metres.
func DistanceM(a, b Point) float64 {
	p1, p2 := rad(a.Lat), rad(b.Lat)
	dp := p2 - p1
	dl := rad(b.Lon - a.Lon)
	h := math.Sin(dp/2)*math.Sin(dp/2) +
		math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(h))
}

// BearingDeg returns the initial compass bearing from a to b, in degrees
// clockwise from north.
func BearingDeg(a, b Point) float64 {
	p1, p2 := rad(a.Lat), rad(b.Lat)
	dl := rad(b.Lon - a.Lon)
	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
	return math.Mod(deg(math.Atan2(y, x))+360, 360)
}

var compass = [8]string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}

// Compass reduces a bearing to one of eight points. Eight is deliberate: on
// foot, "north-east" is actionable and "north-north-east" is not.
func Compass(bearingDeg float64) string {
	i := int(math.Mod(bearingDeg+22.5, 360) / 45)
	return compass[i]
}

// BoundingRadiusM returns a point and radius covering every input circle, so
// several nearby fences can be served by a single upstream query.
func BoundingRadiusM(centres []Point, radiiM []float64) (Point, float64) {
	if len(centres) == 0 {
		return Point{}, 0
	}
	// Centroid, then the furthest circle edge from it. Not the minimal
	// enclosing circle, but close enough and cheap; over-fetching a little
	// costs nothing because the exact filter runs client-side anyway.
	var sumLat, sumLon float64
	for _, c := range centres {
		sumLat += c.Lat
		sumLon += c.Lon
	}
	n := float64(len(centres))
	mid := Point{Lat: sumLat / n, Lon: sumLon / n}

	var max float64
	for i, c := range centres {
		r := 0.0
		if i < len(radiiM) {
			r = radiiM[i]
		}
		if d := DistanceM(mid, c) + r; d > max {
			max = d
		}
	}
	return mid, max
}

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }
