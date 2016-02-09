package ctwatch

import (
	"container/list"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/certificate-transparency/go"
	"github.com/google/certificate-transparency/go/client"
)

type ProcessCallback func(*Scanner, *ct.LogEntry)

// ScannerOptions holds configuration options for the Scanner
type ScannerOptions struct {
	// Number of entries to request in one batch from the Log
	BatchSize int

	// Number of concurrent proecssors to run
	NumWorkers int

	// Number of concurrent fethers to run
	ParallelFetch int

	// Don't print any status messages to stdout
	Quiet bool
}

// Creates a new ScannerOptions struct with sensible defaults
func DefaultScannerOptions() *ScannerOptions {
	return &ScannerOptions{
		BatchSize:     1000,
		NumWorkers:    1,
		ParallelFetch: 1,
		Quiet:         false,
	}
}

// Scanner is a tool to scan all the entries in a CT Log.
type Scanner struct {
	// Base URI of CT log
	LogUri				string

	// Client used to talk to the CT log instance
	logClient			*client.LogClient

	// Configuration options for this Scanner instance
	opts				ScannerOptions

	// Stats
	certsProcessed			int64
}

// fetchRange represents a range of certs to fetch from a CT log
type fetchRange struct {
	start int64
	end   int64
}

// Worker function to process certs.
// Accepts ct.LogEntries over the |entries| channel, and invokes processCert on them.
// Returns true over the |done| channel when the |entries| channel is closed.
func (s *Scanner) processerJob(id int, entries <-chan ct.LogEntry, processCert ProcessCallback, wg *sync.WaitGroup) {
	for entry := range entries {
		atomic.AddInt64(&s.certsProcessed, 1)
		processCert(s, &entry)
	}
	s.Log(fmt.Sprintf("Processor %d finished", id))
	wg.Done()
}

// Worker function for fetcher jobs.
// Accepts cert ranges to fetch over the |ranges| channel, and if the fetch is
// successful sends the individual LeafInputs out into the
// |entries| channel for the processors to chew on.
// Will retry failed attempts to retrieve ranges indefinitely.
// Sends true over the |done| channel when the |ranges| channel is closed.
func (s *Scanner) fetcherJob(id int, ranges <-chan fetchRange, entries chan<- ct.LogEntry, wg *sync.WaitGroup) {
	for r := range ranges {
		success := false
		// TODO(alcutter): give up after a while:
		for !success {
			s.Log(fmt.Sprintf("Fetching entries %d to %d", r.start, r.end))
			logEntries, err := s.logClient.GetEntries(r.start, r.end)
			if err != nil {
				s.Warn(fmt.Sprintf("Problem fetching from log: %s", err.Error()))
				continue
			}
			for _, logEntry := range logEntries {
				logEntry.Index = r.start
				entries <- logEntry
				r.start++
			}
			if r.start > r.end {
				// Only complete if we actually got all the leaves we were
				// expecting -- Logs MAY return fewer than the number of
				// leaves requested.
				success = true
			}
		}
	}
	s.Log(fmt.Sprintf("Fetcher %d finished", id))
	wg.Done()
}

// Returns the smaller of |a| and |b|
func min(a int64, b int64) int64 {
	if a < b {
		return a
	} else {
		return b
	}
}

// Returns the larger of |a| and |b|
func max(a int64, b int64) int64 {
	if a > b {
		return a
	} else {
		return b
	}
}

// Pretty prints the passed in number of |seconds| into a more human readable
// string.
func humanTime(seconds int) string {
	nanos := time.Duration(seconds) * time.Second
	hours := int(nanos / (time.Hour))
	nanos %= time.Hour
	minutes := int(nanos / time.Minute)
	nanos %= time.Minute
	seconds = int(nanos / time.Second)
	s := ""
	if hours > 0 {
		s += fmt.Sprintf("%d hours ", hours)
	}
	if minutes > 0 {
		s += fmt.Sprintf("%d minutes ", minutes)
	}
	if seconds > 0 {
		s += fmt.Sprintf("%d seconds ", seconds)
	}
	return s
}

func (s Scanner) Log(msg string) {
	if !s.opts.Quiet {
		log.Print(s.LogUri + ": " + msg)
	}
}

func (s Scanner) Warn(msg string) {
	log.Print(s.LogUri + ": " + msg)
}

func (s *Scanner) TreeSize() (int64, error) {
	latestSth, err := s.logClient.GetSTH()
	if err != nil {
		return 0, err
	}
	return int64(latestSth.TreeSize), nil
}

func (s *Scanner) Scan(startIndex int64, endIndex int64, processCert ProcessCallback) error {
	s.Log("Starting scan...");

	s.certsProcessed = 0
	startTime := time.Now()
	fetches := make(chan fetchRange, 1000)
	jobs := make(chan ct.LogEntry, 100000)
	/* TODO: only launch ticker goroutine if in verbose mode; kill the goroutine when the scanner finishes
	ticker := time.NewTicker(time.Second)
	go func() {
		for range ticker.C {
			throughput := float64(s.certsProcessed) / time.Since(startTime).Seconds()
			remainingCerts := int64(endIndex) - int64(startIndex) - s.certsProcessed
			remainingSeconds := int(float64(remainingCerts) / throughput)
			remainingString := humanTime(remainingSeconds)
			s.Log(fmt.Sprintf("Processed: %d certs (to index %d). Throughput: %3.2f ETA: %s", s.certsProcessed,
				startIndex+int64(s.certsProcessed), throughput, remainingString))
		}
	}()
	*/

	var ranges list.List
	for start := startIndex; start < int64(endIndex); {
		end := min(start+int64(s.opts.BatchSize), int64(endIndex)) - 1
		ranges.PushBack(fetchRange{start, end})
		start = end + 1
	}
	var fetcherWG sync.WaitGroup
	var processorWG sync.WaitGroup
	// Start processor workers
	for w := 0; w < s.opts.NumWorkers; w++ {
		processorWG.Add(1)
		go s.processerJob(w, jobs, processCert, &processorWG)
	}
	// Start fetcher workers
	for w := 0; w < s.opts.ParallelFetch; w++ {
		fetcherWG.Add(1)
		go s.fetcherJob(w, fetches, jobs, &fetcherWG)
	}
	for r := ranges.Front(); r != nil; r = r.Next() {
		fetches <- r.Value.(fetchRange)
	}
	close(fetches)
	fetcherWG.Wait()
	close(jobs)
	processorWG.Wait()
	s.Log(fmt.Sprintf("Completed %d certs in %s", s.certsProcessed, humanTime(int(time.Since(startTime).Seconds()))))

	return nil
}

// Creates a new Scanner instance using |client| to talk to the log, and taking
// configuration options from |opts|.
func NewScanner(logUri string, client *client.LogClient, opts ScannerOptions) *Scanner {
	var scanner Scanner
	scanner.LogUri = logUri
	scanner.logClient = client
	scanner.opts = opts
	return &scanner
}
