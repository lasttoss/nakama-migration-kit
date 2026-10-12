package plan_test

import (
	"testing"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/plan"
)

func api(players int) plan.Inputs {
	return plan.Inputs{
		Strategy:          "playfab-admin-api",
		Players:           players,
		CallsPerPlayer:    6,
		RequestsPerSecond: 20,
		Concurrency:       8,
		PerCallLatency:    100 * time.Millisecond,
		BatchSize:         250,
		WriteLatency:      20 * time.Millisecond,
	}
}

func dump(players int) plan.Inputs {
	return plan.Inputs{
		Strategy:          "title-export",
		Players:           players,
		CallsPerPlayer:    0,
		RequestsPerSecond: 0,
		BatchSize:         500,
		WriteLatency:      30 * time.Millisecond,
	}
}

// The number a studio actually asks for: ten thousand players, six calls each, against a
// measured rate limit.
func TestTenThousandPlayersAgainstTheAPI(t *testing.T) {
	got := plan.Compute(api(10000))
	if got.SourceCalls != 60000 {
		t.Errorf("calls = %d, want 60000", got.SourceCalls)
	}
	// 60000 calls at 20/s is 3000 s, and the writes are not the constraint.
	if got.WallClockSeconds < 2900 || got.WallClockSeconds > 3100 {
		t.Errorf("wall clock = %.0f s, want about 3000", got.WallClockSeconds)
	}
	if got.Bottleneck != "the source's rate limit" {
		t.Errorf("bottleneck = %q", got.Bottleneck)
	}
}

// The point the plan exists to make: a million players of per-player API calls is days.
func TestAMillionPlayersIsDaysAgainstTheAPI(t *testing.T) {
	got := plan.Compute(api(1000000))
	if got.WallClock < 60*time.Hour {
		t.Errorf("wall clock = %s, want more than 60 h", plan.Human(got.WallClock))
	}
	if plan.Human(got.WallClock) == "0s" {
		t.Error("no estimate was produced")
	}
}

// And the way out is not a faster loop, it is a different source.
func TestTheExportIsHoursWhereTheAPIIsDays(t *testing.T) {
	viaAPI := plan.Compute(api(1000000))
	viaDump := plan.Compute(dump(1000000))
	if viaDump.WallClock >= viaAPI.WallClock/10 {
		t.Errorf("the export (%s) is not an order of magnitude faster than the API (%s)",
			plan.Human(viaDump.WallClock), plan.Human(viaAPI.WallClock))
	}
	if viaDump.TargetBatches != 2000 {
		t.Errorf("batches = %d, want 2000 at 500 per batch", viaDump.TargetBatches)
	}
}

// Little's law: a hundred workers against a 200 ms API are not a hundred requests a second, and
// a plan that ignores that overruns the window it promised.
func TestConcurrencyCanBeTheBottleneck(t *testing.T) {
	in := api(10000)
	in.Concurrency = 2
	in.PerCallLatency = 200 * time.Millisecond

	got := plan.Compute(in)
	if got.Bottleneck != "concurrency (the workers cannot keep the limit busy)" {
		t.Errorf("bottleneck = %q, want the concurrency", got.Bottleneck)
	}
	// 2 workers / 0.2 s = 10 requests a second, half the limit.
	if got.WallClockSeconds < 5900 || got.WallClockSeconds > 6100 {
		t.Errorf("wall clock = %.0f s, want about 6000 (half the limit is usable)", got.WallClockSeconds)
	}
}

// Retries are wall clock, and a title that answers 429 to a twentieth of the calls pays for it.
func TestRetriesAreCounted(t *testing.T) {
	without := plan.Compute(api(10000))
	with := api(10000)
	with.RetryOverhead = 0.20
	slower := plan.Compute(with)

	if slower.SourceCalls <= without.SourceCalls {
		t.Errorf("retries did not add calls: %d vs %d", slower.SourceCalls, without.SourceCalls)
	}
	if slower.WallClockSeconds <= without.WallClockSeconds {
		t.Error("retries did not add wall clock")
	}
}

func TestBatchingChangesTheTargetSideOnly(t *testing.T) {
	small := api(10000)
	small.BatchSize = 50
	large := api(10000)
	large.BatchSize = 500

	a, b := plan.Compute(small), plan.Compute(large)
	if a.SourceCalls != b.SourceCalls {
		t.Errorf("batch size changed the source calls: %d vs %d", a.SourceCalls, b.SourceCalls)
	}
	if a.TargetBatches != 10*b.TargetBatches {
		t.Errorf("batches = %d and %d, want a factor of ten", a.TargetBatches, b.TargetBatches)
	}
}

func TestNothingToMoveIsNotADivisionByZero(t *testing.T) {
	got := plan.Compute(api(0))
	if got.WallClock != 0 || got.PlayersPerSecond != 0 {
		t.Errorf("an empty migration produced %s and %.1f players/s", got.WallClock, got.PlayersPerSecond)
	}
}

// A dump with no rate limit and no writes configured cannot be estimated, and saying so is
// better than printing a confident zero.
func TestAnUnconfiguredRateLimitSaysSo(t *testing.T) {
	in := dump(1000)
	in.WriteLatency = 0
	got := plan.Compute(in)
	if got.Bottleneck == "" || got.Bottleneck == "the source's rate limit" {
		t.Errorf("bottleneck = %q, want it to admit nothing bounds this estimate", got.Bottleneck)
	}
}

func TestHumanReadsLikeAPlan(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0s",
		45 * time.Second: "45.0s",
		10 * time.Minute: "10.0 min",
		5 * time.Hour:    "5.0 h",
		72 * time.Hour:   "3.0 days",
	}
	for d, want := range cases {
		if got := plan.Human(d); got != want {
			t.Errorf("Human(%s) = %q, want %q", d, got, want)
		}
	}
}

// The numbers printed here go into the README, so they are pinned: a change to the formula
// that moves them is a change to what the repository claims.
func TestTheNumbersTheReadmeQuotes(t *testing.T) {
	t.Logf("10k players, Admin API at 20 req/s:        %s (%d calls)", plan.Human(plan.Compute(api(10000)).WallClock), plan.Compute(api(10000)).SourceCalls)
	t.Logf("100k players, Admin API at 20 req/s:       %s", plan.Human(plan.Compute(api(100000)).WallClock))
	t.Logf("1M players, Admin API at 20 req/s:         %s", plan.Human(plan.Compute(api(1000000)).WallClock))
	t.Logf("1M players, title export + batched writes: %s", plan.Human(plan.Compute(dump(1000000)).WallClock))
	t.Logf("1M players, export, 8 writers:             %s", plan.Human(plan.Compute(func() plan.Inputs {
		in := dump(1000000)
		in.WriteLatency = 8 * time.Millisecond
		in.Concurrency = 8
		in.PerCallLatency = 30 * time.Millisecond
		in.RequestsPerSecond = 1000
		return in
	}()).WallClock))
}
