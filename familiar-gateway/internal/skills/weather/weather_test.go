package weather

import "testing"

func TestParseLatLon(t *testing.T) {
	cases := []struct {
		in               string
		wantOK           bool
		wantLat, wantLon float64
	}{
		{"45.4318,-122.3745", true, 45.4318, -122.3745},
		{" 45.4318 , -122.3745 ", true, 45.4318, -122.3745},
		{"0,0", true, 0, 0},
		{"Boring, OR", false, 0, 0},
		{"45.4318", false, 0, 0},
		{"100,0", false, 0, 0}, // lat out of range
		{"0,200", false, 0, 0}, // lon out of range
		{"", false, 0, 0},
	}
	for _, tc := range cases {
		lat, lon, ok := parseLatLon(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseLatLon(%q) ok=%v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && (lat != tc.wantLat || lon != tc.wantLon) {
			t.Errorf("parseLatLon(%q) = %v,%v, want %v,%v", tc.in, lat, lon, tc.wantLat, tc.wantLon)
		}
	}
}

func TestNormalizeLocationKey(t *testing.T) {
	cases := map[string]string{
		"Boring, OR":     "boring or",
		"boring,or":      "boring or",
		"Boring OR":      "boring or",
		"Boring,   OR":   "boring or",
		"  Boring, OR  ": "boring or",
		"Boring, Oregon": "boring oregon",
	}
	for in, want := range cases {
		if got := normalizeLocationKey(in); got != want {
			t.Errorf("normalizeLocationKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnownLocationsHasBoring(t *testing.T) {
	// The primary regression guard: "Boring, OR" must resolve without
	// hitting Open-Meteo's geocoder.
	for _, key := range []string{"boring or", "boring oregon"} {
		loc, ok := knownLocations[key]
		if !ok {
			t.Errorf("knownLocations missing %q", key)
			continue
		}
		if loc.Latitude < 45.3 || loc.Latitude > 45.5 {
			t.Errorf("knownLocations[%q] latitude out of range: %v", key, loc.Latitude)
		}
	}
}
