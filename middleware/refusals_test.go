package middleware

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// A ForbiddenSampler keys its windows by outcome and organization; a sweep
// once a minute drops the pairs whose last line is older than that, handing
// on the count of refusals they hadn't logged; past maxKeys, a new pair's
// 403s are counted together, a line a minute. Two organizations' 403s of
// one outcome in the same minute are two lines.
func TestTheForbiddenSamplerIsPerOrganizationAndBounded(t *testing.T) {
	clock := newTestClock()
	var flushes []ForbiddenFlush
	s := newForbiddenSampler(clock.Now, func(f ForbiddenFlush) { flushes = append(flushes, f) }, 3)
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	note := func(organization uuid.UUID) (bool, int, bool) {
		t.Helper()
		return s.Note(outcomeArchived, organization)
	}

	// Two organizations, one outcome, one minute: a line each.
	if log, count, full := note(a); !log || count != 1 || full {
		t.Fatalf("a's first 403 = %v, %d, %v; want logged, 1", log, count, full)
	}
	if log, count, full := note(b); !log || count != 1 || full {
		t.Fatalf("b's first 403 in the same minute = %v, %d, %v; want logged, 1", log, count, full)
	}
	for range 3 {
		if log, _, _ := note(a); log {
			t.Fatal("a's next 403 in the same minute was logged")
		}
	}
	if log, _, _ := s.Note(outcomeFleetType, a); !log {
		t.Fatal("a's 403 of another outcome was not logged")
	}

	// Full (a's and b's archived, a's fleet type: past maxKeys 3, new pairs
	// are counted together): c's is a line saying so, d's in the same
	// minute none.
	if log, count, full := note(c); !log || count != 1 || !full {
		t.Fatalf("c's 403, the sampler full = %v, %d, %v; want logged, 1, full", log, count, full)
	}
	if log, _, full := note(d); log || !full {
		t.Fatalf("d's 403, the sampler full, the same minute = %v, full %v; want not logged", log, full)
	}
	if len(s.windows) != 3 {
		t.Fatalf("%d pairs tracked; want 3", len(s.windows))
	}

	// A minute on, c's 403 sweeps: a's three unlogged refusals are handed
	// on, and the full sampler's one (d's); b's and a's fleet type's
	// (nothing unlogged) dropped quietly, and c is tracked.
	clock.Advance(time.Minute)
	if log, count, full := note(c); !log || count != 1 || full {
		t.Fatalf("c's 403 a minute on = %v, %d, %v; want logged, 1, tracked", log, count, full)
	}
	if len(flushes) != 2 || flushes[0] != (ForbiddenFlush{Outcome: outcomeArchived, Organization: a, Count: 3}) ||
		flushes[1] != (ForbiddenFlush{Count: 1, Full: true}) {
		t.Fatalf("the sweep handed on %v; want a's 3 unlogged archived 403s, then the full sampler's 1", flushes)
	}
	if _, kept := s.windows[forbiddenKey{outcomeArchived, b}]; kept || len(s.windows) != 1 {
		t.Fatalf("after the sweep, pairs %v; want c's only", s.windows)
	}
}

// The sweep flushes the full sampler's count too. The sampler full, new
// pairs' 403s are counted together; a minute on, the old pairs swept away,
// a new pair has room again and is tracked, so the full sampler's count
// would never be noted again (stranded). The sweep hands it on, once,
// naming no outcome or organization; a full 403 after it is a line again.
func TestTheSweepFlushesTheFullSamplersCount(t *testing.T) {
	clock := newTestClock()
	var flushes []ForbiddenFlush
	s := newForbiddenSampler(clock.Now, func(f ForbiddenFlush) { flushes = append(flushes, f) }, 1)
	tracked := uuid.New()
	if log, _, full := s.Note(outcomeArchived, tracked); !log || full {
		t.Fatal("the first pair's 403 was not logged, tracked")
	}
	for i := range 5 {
		log, _, full := s.Note(outcomeArchived, uuid.New())
		if !full || log != (i == 0) {
			t.Fatalf("new pair %d, the sampler full: logged %v, full %v; want full, the first logged", i, log, full)
		}
	}
	clock.Advance(time.Minute)
	fresh := uuid.New()
	if log, count, full := s.Note(outcomeFleetType, fresh); !log || count != 1 || full {
		t.Fatalf("a new pair a minute on = %v, %d, %v; want logged, 1, tracked", log, count, full)
	}
	if len(flushes) != 1 || flushes[0] != (ForbiddenFlush{Count: 4, Full: true}) {
		t.Fatalf("the sweep handed on %v; want the full sampler's 4 unlogged 403s", flushes)
	}
	if attrs := flushes[0].Attrs(); len(attrs) != 6 || attrs[0] != "refused_since_last_log" || attrs[3] != true {
		t.Fatalf("the full flush's attributes %v; want the count and sampler_full, no outcome or organization", attrs)
	}
	if log, count, full := s.Note(outcomeArchived, uuid.New()); !log || count != 1 || !full {
		t.Fatalf("a full 403 after the flush, the same minute = %v, %d, %v; want logged, 1, full", log, count, full)
	}
	if len(flushes) != 1 {
		t.Fatalf("flushed again: %v", flushes)
	}
}
