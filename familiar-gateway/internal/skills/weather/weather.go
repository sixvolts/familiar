// Package weather provides a Familiar skill backed by Pirate Weather for
// conditions and Open-Meteo for geocoding.
//
// Pirate Weather is a drop-in replacement for the Dark Sky API and provides
// rich hour-by-hour and day-by-day blocks from one endpoint. It takes
// lat/lon only — no place name lookup — so we keep Open-Meteo's free
// geocoder (plus a tiny knownLocations table for places it misses) as the
// front door.
//
// A small in-process TTL cache sits in front of both APIs. It's
// intentionally naive (one map, one mutex, lazy expiry) because weather
// lookups are low-volume.
package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/skills"
)

const (
	geocodeEndpoint       = "https://geocoding-api.open-meteo.com/v1/search"
	pirateWeatherEndpoint = "https://api.pirateweather.net/forecast"

	defaultCurrentTTL  = 10 * time.Minute
	defaultForecastTTL = 30 * time.Minute
)

// Skill exposes get_current_weather and get_forecast via Pirate Weather.
type Skill struct {
	http   *http.Client
	cache  *ttlCache
	apiKey string
}

// New constructs a weather skill. Pass a custom http.Client to control
// timeouts/proxies; nil uses a 10s default. apiKey is the Pirate Weather
// API key — when empty, Execute returns a configuration error instead of
// hitting the network.
func New(httpClient *http.Client, apiKey string) *Skill {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Skill{
		http:   httpClient,
		cache:  newTTLCache(),
		apiKey: apiKey,
	}
}

func (s *Skill) Name() string { return "weather" }
func (s *Skill) Description() string {
	return "Current weather and short-range forecasts via Pirate Weather"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

var currentWeatherParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "location": {
      "type": "string",
      "description": "City name or \"lat,lon\" coordinates. Examples: \"Tokyo\", \"Paris\", \"35.68,139.69\". If a city name fails, retry with coordinates."
    }
  },
  "required": ["location"]
}`)

var forecastParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "location": {
      "type": "string",
      "description": "City or place name."
    },
    "days": {
      "type": "integer",
      "description": "Number of forecast days (1-7). Defaults to 3.",
      "minimum": 1,
      "maximum": 7
    }
  },
  "required": ["location"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "get_current_weather",
			Description: "Get current weather conditions for a location.",
			Parameters:  currentWeatherParams,
		},
		{
			Name:        "get_forecast",
			Description: "Get a multi-day weather forecast for a location (1-7 days).",
			Parameters:  forecastParams,
		},
	}
}

type currentArgs struct {
	Location string `json:"location"`
}

type forecastArgs struct {
	Location string `json:"location"`
	Days     *int   `json:"days,omitempty"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	switch toolName {
	case "get_current_weather":
		var args currentArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if args.Location == "" {
			return skills.ToolResult{Error: "location is required"}, nil
		}
		return s.currentWeather(ctx, args.Location)

	case "get_forecast":
		var args forecastArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if args.Location == "" {
			return skills.ToolResult{Error: "location is required"}, nil
		}
		days := 3
		if args.Days != nil {
			days = *args.Days
		}
		if days < 1 {
			days = 1
		}
		if days > 7 {
			days = 7
		}
		return s.forecast(ctx, args.Location, days)

	default:
		return skills.ToolResult{}, fmt.Errorf("weather: unknown tool %q", toolName)
	}
}

// --- geocoding --------------------------------------------------------------

type geocodeResult struct {
	Name      string  `json:"name"`
	Admin1    string  `json:"admin1"`
	Country   string  `json:"country"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
}

type geocodeResponse struct {
	Results []geocodeResult `json:"results"`
}

// knownLocations is a small hand-maintained table of coordinates for
// places the Open-Meteo geocoder can't find. Open-Meteo's geocoder fails
// on "Boring, OR" and "Boring, Oregon" despite Boring being a real town —
// a city name that is also a common word is the canonical case that
// motivates this table.
// Keys are lowercased; normalization strips punctuation.
var knownLocations = map[string]geocodeResult{
	"boring or": {
		Name: "Boring", Admin1: "Oregon", Country: "United States",
		Latitude: 45.4318, Longitude: -122.3745, Timezone: "America/Los_Angeles",
	},
	"boring oregon": {
		Name: "Boring", Admin1: "Oregon", Country: "United States",
		Latitude: 45.4318, Longitude: -122.3745, Timezone: "America/Los_Angeles",
	},
}

// parseLatLon tries to parse a "lat,lon" coordinate string. Returns ok=false
// if either component is missing or non-numeric. Accepts optional whitespace.
func parseLatLon(s string) (lat, lon float64, ok bool) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	lon, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, false
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}

// normalizeLocationKey lowercases and collapses punctuation so
// "Boring, OR" / "Boring OR" / "boring,or" all hit the same map entry.
func normalizeLocationKey(location string) string {
	s := strings.ToLower(location)
	s = strings.ReplaceAll(s, ",", " ")
	s = strings.ReplaceAll(s, ".", " ")
	return strings.Join(strings.Fields(s), " ")
}

// geocode resolves a location string to coordinates. Resolution order:
//  1. Raw "lat,lon" — bypass the geocoder entirely.
//  2. knownLocations table — hardcoded fallback for places Open-Meteo misses.
//  3. Open-Meteo geocoding API with the full location string.
//  4. Retry with the head component before the first comma (e.g.
//     "Boring, OR" -> "Boring"), which frequently succeeds when the state
//     suffix confuses the geocoder.
func (s *Skill) geocode(ctx context.Context, location string) (*geocodeResult, error) {
	if lat, lon, ok := parseLatLon(location); ok {
		return &geocodeResult{
			Name:      fmt.Sprintf("%.4f,%.4f", lat, lon),
			Latitude:  lat,
			Longitude: lon,
			Timezone:  "auto",
		}, nil
	}

	if hit, ok := knownLocations[normalizeLocationKey(location)]; ok {
		r := hit
		return &r, nil
	}

	if r, err := s.geocodeRemote(ctx, location); err == nil {
		return r, nil
	} else if head := strings.TrimSpace(strings.SplitN(location, ",", 2)[0]); head != "" && head != location {
		// Retry with just the city name. If that also fails, return the
		// original error so callers see the actual geocoder complaint.
		if r2, err2 := s.geocodeRemote(ctx, head); err2 == nil {
			return r2, nil
		}
		return nil, err
	} else {
		return nil, err
	}
}

// geocodeRemote is the raw Open-Meteo call. Kept separate from geocode()
// so the fallback logic can invoke it multiple times cheaply.
func (s *Skill) geocodeRemote(ctx context.Context, location string) (*geocodeResult, error) {
	q := url.Values{}
	q.Set("name", location)
	q.Set("count", "1")
	q.Set("language", "en")
	q.Set("format", "json")

	req, err := http.NewRequestWithContext(ctx, "GET", geocodeEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("geocode %d: %s", resp.StatusCode, string(body))
	}
	var out geocodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 {
		return nil, fmt.Errorf("no location match for %q", location)
	}
	return &out.Results[0], nil
}

// --- Pirate Weather ---------------------------------------------------------

// pirateCurrently is the `currently` block from a Pirate Weather forecast.
// Only the fields we render are parsed; the API returns many more.
type pirateCurrently struct {
	Time                int64   `json:"time"`
	Summary             string  `json:"summary"`
	Icon                string  `json:"icon"`
	Temperature         float64 `json:"temperature"`
	ApparentTemperature float64 `json:"apparentTemperature"`
	Humidity            float64 `json:"humidity"` // 0..1
	WindSpeed           float64 `json:"windSpeed"`
	WindGust            float64 `json:"windGust"`
	WindBearing         float64 `json:"windBearing"` // degrees, 0=N

	PrecipIntensity   float64 `json:"precipIntensity"`
	PrecipProbability float64 `json:"precipProbability"` // 0..1
	PrecipType        string  `json:"precipType"`
}

type pirateDailyPoint struct {
	Time               int64   `json:"time"`
	Summary            string  `json:"summary"`
	Icon               string  `json:"icon"`
	TemperatureHigh    float64 `json:"temperatureHigh"`
	TemperatureLow     float64 `json:"temperatureLow"`
	WindSpeed          float64 `json:"windSpeed"`
	WindGust           float64 `json:"windGust"`
	PrecipAccumulation float64 `json:"precipAccumulation"`
	PrecipIntensity    float64 `json:"precipIntensity"`
	PrecipProbability  float64 `json:"precipProbability"`
	PrecipType         string  `json:"precipType"`
}

type pirateDaily struct {
	Summary string             `json:"summary"`
	Icon    string             `json:"icon"`
	Data    []pirateDailyPoint `json:"data"`
}

// pirateHourlyPoint is one bucket from the `hourly` block. Only the
// fields the Home weather widget renders are parsed.
type pirateHourlyPoint struct {
	Time        int64   `json:"time"`
	Icon        string  `json:"icon"`
	Temperature float64 `json:"temperature"`
}

type pirateHourly struct {
	Summary string              `json:"summary"`
	Icon    string              `json:"icon"`
	Data    []pirateHourlyPoint `json:"data"`
}

type pirateResponse struct {
	Latitude  float64         `json:"latitude"`
	Longitude float64         `json:"longitude"`
	Timezone  string          `json:"timezone"`
	Currently pirateCurrently `json:"currently"`
	Hourly    pirateHourly    `json:"hourly"`
	Daily     pirateDaily     `json:"daily"`
}

// fetchPirate calls the Pirate Weather forecast endpoint for a set of
// coordinates. exclude trims down the payload; "us" units give Fahrenheit
// and mph to match the previous Open-Meteo formatting.
func (s *Skill) fetchPirate(ctx context.Context, lat, lon float64, exclude string) (*pirateResponse, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("pirate weather api key not configured")
	}
	path := fmt.Sprintf("%s/%s/%f,%f", pirateWeatherEndpoint, s.apiKey, lat, lon)
	q := url.Values{}
	q.Set("units", "us")
	if exclude != "" {
		q.Set("exclude", exclude)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pirate weather %d: %s", resp.StatusCode, string(body))
	}
	var out pirateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- current weather --------------------------------------------------------

func (s *Skill) currentWeather(ctx context.Context, location string) (skills.ToolResult, error) {
	key := "current:" + strings.ToLower(location)
	if hit, ok := s.cache.get(key); ok {
		return skills.ToolResult{Content: hit.content, Data: hit.data, Tokens: len(hit.content) / 4, Cached: true}, nil
	}

	loc, err := s.geocode(ctx, location)
	if err != nil {
		return skills.ToolResult{Error: "geocode failed: " + err.Error()}, nil
	}

	out, err := s.fetchPirate(ctx, loc.Latitude, loc.Longitude, "minutely,hourly,daily,alerts,flags")
	if err != nil {
		return skills.ToolResult{Error: err.Error()}, nil
	}

	content := formatCurrent(loc, out.Currently)
	data, _ := json.Marshal(map[string]any{
		"location":  loc,
		"currently": out.Currently,
	})
	s.cache.set(key, cacheEntry{content: content, data: data}, defaultCurrentTTL)
	return skills.ToolResult{Content: content, Data: data, Tokens: len(content) / 4}, nil
}

// --- forecast ---------------------------------------------------------------

func (s *Skill) forecast(ctx context.Context, location string, days int) (skills.ToolResult, error) {
	key := fmt.Sprintf("forecast:%d:%s", days, strings.ToLower(location))
	if hit, ok := s.cache.get(key); ok {
		return skills.ToolResult{Content: hit.content, Data: hit.data, Tokens: len(hit.content) / 4, Cached: true}, nil
	}

	loc, err := s.geocode(ctx, location)
	if err != nil {
		return skills.ToolResult{Error: "geocode failed: " + err.Error()}, nil
	}

	out, err := s.fetchPirate(ctx, loc.Latitude, loc.Longitude, "minutely,hourly,alerts,flags")
	if err != nil {
		return skills.ToolResult{Error: err.Error()}, nil
	}

	points := out.Daily.Data
	if len(points) > days {
		points = points[:days]
	}

	content := formatForecast(loc, out.Daily.Summary, points)
	data, _ := json.Marshal(map[string]any{
		"location": loc,
		"daily": map[string]any{
			"summary": out.Daily.Summary,
			"icon":    out.Daily.Icon,
			"data":    points,
		},
	})
	s.cache.set(key, cacheEntry{content: content, data: data}, defaultForecastTTL)
	return skills.ToolResult{Content: content, Data: data, Tokens: len(content) / 4}, nil
}

// --- formatting -------------------------------------------------------------

// iconText maps Pirate Weather icon strings (a superset of Dark Sky's)
// to short English labels. Unknown icons pass through verbatim.
// See https://docs.pirateweather.net/en/latest/API/#forecast-request.
func iconText(icon string) string {
	switch icon {
	case "clear-day", "clear-night":
		return "clear"
	case "partly-cloudy-day", "partly-cloudy-night":
		return "partly cloudy"
	case "cloudy":
		return "cloudy"
	case "rain":
		return "rain"
	case "snow":
		return "snow"
	case "sleet":
		return "sleet"
	case "wind":
		return "windy"
	case "fog":
		return "fog"
	case "hail":
		return "hail"
	case "thunderstorm":
		return "thunderstorm"
	case "tornado":
		return "tornado"
	case "":
		return "unknown"
	default:
		return icon
	}
}

func locationLabel(loc *geocodeResult) string {
	parts := []string{loc.Name}
	if loc.Admin1 != "" {
		parts = append(parts, loc.Admin1)
	}
	if loc.Country != "" {
		parts = append(parts, loc.Country)
	}
	return strings.Join(parts, ", ")
}

func formatCurrent(loc *geocodeResult, c pirateCurrently) string {
	when := time.Unix(c.Time, 0).UTC().Format(time.RFC3339)
	return fmt.Sprintf(
		"Current weather in %s: %.0f°F (feels like %.0f°F), %s. Humidity %.0f%%, wind %.0f mph (gust %.0f), precip %.2f in/hr. As of %s.",
		locationLabel(loc),
		c.Temperature, c.ApparentTemperature,
		iconText(c.Icon),
		c.Humidity*100, c.WindSpeed, c.WindGust, c.PrecipIntensity,
		when,
	)
}

func formatForecast(loc *geocodeResult, summary string, points []pirateDailyPoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Forecast for %s", locationLabel(loc))
	if summary != "" {
		fmt.Fprintf(&b, " — %s", summary)
	}
	b.WriteString(":\n")
	for _, p := range points {
		day := time.Unix(p.Time, 0).UTC().Format("2006-01-02")
		fmt.Fprintf(&b, "- %s: %s, high %.0f°F / low %.0f°F, precip %.2f in (%.0f%%), wind up to %.0f mph\n",
			day,
			iconText(p.Icon),
			p.TemperatureHigh, p.TemperatureLow,
			p.PrecipAccumulation, p.PrecipProbability*100,
			p.WindSpeed,
		)
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- Home weather widget ----------------------------------------------------

// HomeReport is the structured payload for the workspace Home weather
// widget — current conditions, today's high/low, a short description,
// and the next few hourly buckets. Unlike the LLM-facing tools it
// returns typed data instead of prose.
type HomeReport struct {
	TempF       float64    `json:"temp_f"`
	FeelsF      float64    `json:"feels_f"`
	Condition   string     `json:"condition"`   // friendly label, e.g. "partly cloudy"
	Icon        string     `json:"icon"`        // raw Pirate Weather icon string
	Description string     `json:"description"` // hourly (or daily) summary sentence
	WindMph     float64    `json:"wind_mph"`
	WindDir     string     `json:"wind_dir"` // 8-point compass, e.g. "NW"
	HighF       float64    `json:"high_f"`
	LowF        float64    `json:"low_f"`
	Hourly      []HomeHour `json:"hourly"`
}

// HomeHour is one bucket in HomeReport.Hourly.
type HomeHour struct {
	Time  int64   `json:"time"` // unix seconds
	TempF float64 `json:"temp_f"`
	Icon  string  `json:"icon"`
}

// HomeForecast returns the Home weather widget payload for an exact
// coordinate pair (e.g. browser geolocation). See homeForecast for
// the shared fetch + reshape + cache core.
func (s *Skill) HomeForecast(ctx context.Context, lat, lon float64) (*HomeReport, error) {
	return s.homeForecast(ctx, lat, lon)
}

// HomeForecastByLocation geocodes a human place string ("Boring, OR",
// a city name, or raw "lat,lon") and returns the Home weather widget
// payload for the resolved coordinates. Used when coordinates aren't
// supplied and the caller's stored profile location drives the widget.
func (s *Skill) HomeForecastByLocation(ctx context.Context, location string) (*HomeReport, error) {
	loc, err := s.geocode(ctx, location)
	if err != nil {
		return nil, fmt.Errorf("geocode failed: %w", err)
	}
	return s.homeForecast(ctx, loc.Latitude, loc.Longitude)
}

// homeForecast fetches current conditions plus the hourly + daily
// blocks for a coordinate pair and reshapes them for the Home weather
// widget. It bypasses the prose formatters and caches the reshaped
// report keyed on coarsely-rounded coordinates. Returns up to 8
// hourly buckets starting from the current hour.
func (s *Skill) homeForecast(ctx context.Context, lat, lon float64) (*HomeReport, error) {
	key := fmt.Sprintf("home:%.2f,%.2f", lat, lon)
	if hit, ok := s.cache.get(key); ok && len(hit.data) > 0 {
		var cached HomeReport
		if err := json.Unmarshal(hit.data, &cached); err == nil {
			return &cached, nil
		}
	}

	out, err := s.fetchPirate(ctx, lat, lon, "minutely,alerts,flags")
	if err != nil {
		return nil, err
	}
	rep := &HomeReport{
		TempF:     out.Currently.Temperature,
		FeelsF:    out.Currently.ApparentTemperature,
		Condition: iconText(out.Currently.Icon),
		Icon:      out.Currently.Icon,
		WindMph:   out.Currently.WindSpeed,
		WindDir:   compass(out.Currently.WindBearing),
	}
	if len(out.Daily.Data) > 0 {
		rep.HighF = out.Daily.Data[0].TemperatureHigh
		rep.LowF = out.Daily.Data[0].TemperatureLow
	}
	if out.Hourly.Summary != "" {
		rep.Description = out.Hourly.Summary
	} else {
		rep.Description = out.Daily.Summary
	}
	for i, h := range out.Hourly.Data {
		if i >= 8 {
			break
		}
		rep.Hourly = append(rep.Hourly, HomeHour{
			Time: h.Time, TempF: h.Temperature, Icon: h.Icon,
		})
	}
	if blob, err := json.Marshal(rep); err == nil {
		s.cache.set(key, cacheEntry{data: blob}, defaultForecastTTL)
	}
	return rep, nil
}

// compass maps a wind bearing in degrees to an 8-point compass label.
func compass(bearing float64) string {
	dirs := []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}
	idx := int((bearing+22.5)/45.0) & 7
	return dirs[idx]
}

// --- tiny TTL cache ---------------------------------------------------------

type cacheEntry struct {
	content string
	data    json.RawMessage
}

type cacheRecord struct {
	entry   cacheEntry
	expires time.Time
}

type ttlCache struct {
	mu sync.Mutex
	m  map[string]cacheRecord
}

func newTTLCache() *ttlCache {
	return &ttlCache{m: make(map[string]cacheRecord)}
}

func (c *ttlCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.m[key]
	if !ok {
		return cacheEntry{}, false
	}
	if time.Now().After(rec.expires) {
		delete(c.m, key)
		return cacheEntry{}, false
	}
	return rec.entry, true
}

func (c *ttlCache) set(key string, entry cacheEntry, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = cacheRecord{entry: entry, expires: time.Now().Add(ttl)}
}
