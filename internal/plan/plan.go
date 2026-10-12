// Package plan answers the question a studio asks before agreeing to a migration: how long.
//
// The answer is arithmetic, not optimism. A player costs one profile call, one user data call,
// one inventory call, one currency call and one statistics call, so N players is about 6N
// requests against an Admin API that rate limits the whole title. Two facts follow, and both are
// things a migration plan has to say out loud:
//
//   - the wall clock is bounded by the source's rate limit, not by the importer: nothing in this
//     kit makes a million players of per-player API calls fast, and a plan that claims otherwise
//     is a plan that will overrun the maintenance window;
//   - the way to make it fast is to stop calling the API per player. PlayFab exports a title's
//     data to a warehouse, and reading one export is hours instead of days, which is why the
//     Source interface has a second implementation for a dump rather than a faster loop for the
//     API.
//
// What is left for the kit is to make the number honest and the run survivable: print the
// estimate before starting, checkpoint so a twenty-hour run that dies at hour nineteen resumes
// instead of restarting, and stay just under the rate limit rather than discovering it.
package plan

import (
	"fmt"
	"time"
)

// Inputs describe one way of reading a source.
type Inputs struct {
	// Strategy names the path: "playfab-admin-api" or "title-export".
	Strategy string `json:"strategy"`
	// Players is how many players have to move.
	Players int `json:"players"`
	// CallsPerPlayer is how many requests the strategy needs per player. A dump needs zero.
	CallsPerPlayer int `json:"calls_per_player"`
	// RequestsPerSecond is the rate limit the source allows for this title. Taken from the
	// title's own configuration, not from the documentation, because it is per title.
	RequestsPerSecond float64 `json:"requests_per_second"`
	// Concurrency is how many requests are in flight. Bounded concurrency is what turns a
	// rate limit into throughput; unbounded concurrency produces 429s, which cost more wall
	// clock than the limit saves.
	Concurrency int `json:"concurrency"`
	// PerCallLatency is the observed round trip for one call.
	PerCallLatency time.Duration `json:"per_call_latency"`
	// RetryOverhead is the fraction of calls that get a 429 or a 5xx and have to be repeated,
	// as a fraction of 1 (0.05 = 5%).
	RetryOverhead float64 `json:"retry_overhead"`
	// BatchSize is how many players go into one write to the target. It does not change how
	// many calls the source needs; it changes how many round trips the target sees.
	BatchSize int `json:"batch_size"`
	// WriteLatency is the observed round trip for one batched write to the target.
	WriteLatency time.Duration `json:"write_latency"`
}

// Estimate is what a strategy costs.
type Estimate struct {
	Strategy string `json:"strategy"`
	// SourceCalls is the number of calls to the legacy backend, retries included.
	SourceCalls int `json:"source_calls"`
	// SourceSeconds is the wall clock the source allows for those calls.
	SourceSeconds float64 `json:"source_seconds"`
	// TargetBatches is how many batch writes the target sees.
	TargetBatches int `json:"target_batches"`
	// TargetSeconds is the wall clock those writes need if they are the only constraint.
	TargetSeconds float64 `json:"target_seconds"`
	// PlayersPerSecond is the throughput of the whole run, bounded by whichever side is slower.
	PlayersPerSecond float64 `json:"players_per_second"`
	// WallClock is the estimate, retries included.
	WallClock time.Duration `json:"-"`
	// WallClockSeconds is the same number in seconds, for the report.
	WallClockSeconds float64 `json:"wall_clock_seconds"`
	// Bottleneck names the side that decides the wall clock.
	Bottleneck string `json:"bottleneck"`
}

// Compute estimates one strategy.
//
// The source is the sum of two ceilings, and the effective rate is the lower one:
//
//   - the rate limit, in requests per second;
//   - what the concurrency can actually keep in flight, which is concurrency divided by the
//     latency of one call (Little's law). A hundred workers against a 200 ms API do not give
//     five hundred requests a second; they give four, and the plan has to say so.
func Compute(in Inputs) Estimate {
	calls := float64(in.Players*in.CallsPerPlayer) * (1 + in.RetryOverhead)
	calls = float64(int(calls + 0.5))

	est := Estimate{Strategy: in.Strategy, SourceCalls: int(calls)}
	if in.Players == 0 {
		est.Bottleneck = "nothing to move"
		return est
	}

	sourceRPS := in.RequestsPerSecond
	if in.Concurrency > 0 && in.PerCallLatency > 0 {
		latencyRPS := float64(in.Concurrency) / in.PerCallLatency.Seconds()
		if latencyRPS < sourceRPS {
			est.Bottleneck = "concurrency (the workers cannot keep the limit busy)"
			sourceRPS = latencyRPS
		}
	}
	bounded := false
	if sourceRPS > 0 {
		est.SourceSeconds = calls / sourceRPS
		bounded = true
	} else if in.CallsPerPlayer > 0 {
		est.Bottleneck = "the source (no rate limit configured: nothing can be estimated)"
	}

	batch := in.BatchSize
	if batch <= 0 {
		batch = 1
	}
	est.TargetBatches = (in.Players + batch - 1) / batch
	if in.WriteLatency > 0 {
		est.TargetSeconds = float64(est.TargetBatches) * in.WriteLatency.Seconds()
	}

	seconds := est.SourceSeconds
	if est.TargetSeconds > seconds {
		est.Bottleneck = "the target (batch writes are the slower side)"
		seconds = est.TargetSeconds
		bounded = true
	}
	// Saying "the source's rate limit" when no rate limit was given would be a confident
	// number built on nothing. A plan that cannot be estimated says so.
	if est.Bottleneck == "" {
		if bounded {
			est.Bottleneck = "the source's rate limit"
		} else {
			est.Bottleneck = "nothing bounds this estimate: set a rate limit, a call latency or a write latency"
		}
	}
	est.WallClockSeconds = seconds
	est.WallClock = time.Duration(seconds * float64(time.Second))
	if seconds > 0 {
		est.PlayersPerSecond = float64(in.Players) / seconds
	}
	return est
}

// Human renders a duration the way a migration plan reads: hours, or days.
func Human(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.1f min", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	default:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	}
}
