package utils

import (
	"sync"
	"testing"
	"time"
)

// The first occurrence has to be reported at once. A counter that waits a
// second before saying anything turns a one-off failure into silence, which is
// the behaviour this type exists to replace.
func TestTallyReportsTheFirstOneImmediately(t *testing.T) {
	var tl Tally
	if n := tl.Note(); n != 1 {
		t.Errorf("first occurrence reported %d, want 1", n)
	}
}

// A flood must collapse into one line carrying the count, not vanish and not
// print a thousand times.
func TestTallyPoolsAFlood(t *testing.T) {
	var tl Tally
	tl.Note() // consumes the immediate first report

	reported := 0
	for i := 0; i < 500; i++ {
		if n := tl.Note(); n > 0 {
			reported += n
		}
	}
	if reported != 0 {
		t.Errorf("a burst inside one second reported %d times, want none", reported)
	}
}

// Nothing may be lost: whatever happened between two reports is carried by the
// next one, so the numbers in the log add up to what actually occurred.
func TestTallyCarriesEverythingItSwallowed(t *testing.T) {
	var tl Tally
	tl.Note()

	const burst = 40
	for i := 0; i < burst; i++ {
		tl.Note()
	}
	time.Sleep(1050 * time.Millisecond)

	n := tl.Note()
	if n != burst+1 {
		t.Errorf("the next report carried %d, want %d — %d occurrences were lost",
			n, burst+1, burst+1-n)
	}
}

// It is written to from every goroutine that fails a send, so counting must
// hold up under that.
func TestTallyCountsUnderConcurrency(t *testing.T) {
	var tl Tally
	tl.Note()

	var wg sync.WaitGroup
	const writers, each = 8, 100
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				tl.Note()
			}
		}()
	}
	wg.Wait()
	time.Sleep(1050 * time.Millisecond)

	if n := tl.Note(); n != writers*each+1 {
		t.Errorf("counted %d, want %d", n, writers*each+1)
	}
}
