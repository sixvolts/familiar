package actions

import (
	"testing"
	"time"
)

func mustSchedule(t *testing.T, spec, zone string) interface{ Next(time.Time) time.Time } {
	t.Helper()
	s, err := scheduleFor(&Action{Name: "t", ID: "t", Cron: spec, Timezone: zone})
	if err != nil {
		t.Fatalf("scheduleFor(%q, %q): %v", spec, zone, err)
	}
	return s
}

// fires returns the first n fire times after from, in loc.
func fires(s interface{ Next(time.Time) time.Time }, from time.Time, n int, loc *time.Location) []string {
	var out []string
	t := from
	for i := 0; i < n; i++ {
		t = s.Next(t)
		out = append(out, t.In(loc).Format("2006-01-02 15:04 MST"))
	}
	return out
}

// A "7am UTC" action fires at 07:00 UTC whatever the host's zone. It used
// to be evaluated in time.Local and fire at 7am host time.
func TestSchedule_UTCIgnoresHostZone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	saved := time.Local
	time.Local = chicago
	defer func() { time.Local = saved }()

	s := mustSchedule(t, "0 7 * * *", "UTC")
	from := time.Date(2026, 9, 23, 0, 0, 0, 0, chicago)
	got := s.Next(from).UTC()
	want := time.Date(2026, 9, 23, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v (it ran on the host's clock)", got, want)
	}
}

func TestSchedule_DSTTransitions(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, spec string
		from       time.Time
		want       []string
	}{
		{
			// 01:30 happens twice on 2026-11-01; a daily job runs once.
			name: "fall back, fixed time runs once",
			spec: "30 1 * * *", from: time.Date(2026, 10, 31, 12, 0, 0, 0, ny),
			want: []string{"2026-11-01 01:30 EDT", "2026-11-02 01:30 EST"},
		},
		{
			// 02:30 doesn't exist on 2026-03-08; the job runs when the
			// clocks jump (03:00) instead of skipping the day.
			name: "spring forward, time in the gap runs at the jump",
			spec: "30 2 * * *", from: time.Date(2026, 3, 7, 12, 0, 0, 0, ny),
			want: []string{"2026-03-08 03:00 EDT", "2026-03-09 02:30 EDT"},
		},
		{
			// An hourly job keeps running on elapsed time through the
			// repeated hour.
			name: "fall back, hourly keeps elapsed time",
			spec: "0 * * * *", from: time.Date(2026, 11, 1, 0, 30, 0, 0, ny),
			want: []string{"2026-11-01 01:00 EDT", "2026-11-01 01:00 EST", "2026-11-01 02:00 EST"},
		},
		{
			// Times away from a transition are unaffected.
			name: "ordinary day",
			spec: "15 9 * * *", from: time.Date(2026, 6, 1, 0, 0, 0, 0, ny),
			want: []string{"2026-06-01 09:15 EDT", "2026-06-02 09:15 EDT"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fires(mustSchedule(t, c.spec, "America/New_York"), c.from, len(c.want), ny)
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("fires = %v, want %v", got, c.want)
				}
			}
		})
	}
}
