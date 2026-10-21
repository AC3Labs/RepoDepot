package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"repodepot/internal/spec"
)

const maxLogLines = 3000

type jobLog struct {
	mu    sync.Mutex
	lines []string
}

// Printf adds a line to the job's log (what the server shows the user).
func (l *jobLog) Printf(format string, args ...any) {
	l.add(fmt.Sprintf(format, args...))
}

func (l *jobLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line = strings.TrimRight(line, "\r\n")
	if len(l.lines) >= maxLogLines {
		l.lines = append(l.lines[:maxLogLines-1], "… (log truncated)")
		return
	}
	l.lines = append(l.lines, line)
}

// Write lets command output stream into the log line by line.
func (l *jobLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			l.add("  " + line)
		}
	}
	return len(p), nil
}

func (l *jobLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

type job struct {
	mu    sync.Mutex
	state spec.JobState
	app   string
	log   *jobLog
}

type jobBook struct {
	mu   sync.Mutex
	jobs map[string]*job
	keep []string // ids, oldest first
}

func newJobBook() *jobBook { return &jobBook{jobs: map[string]*job{}} }

// busy: a job for app is running (any app when app is "").
func (b *jobBook) busy(app string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, j := range b.jobs {
		j.mu.Lock()
		running := (app == "" || j.app == app) && j.state.Status == "running"
		j.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

func (b *jobBook) start(app string, fn func(*jobLog) (string, error)) spec.JobState {
	id := make([]byte, 12)
	rand.Read(id)
	j := &job{app: app, log: &jobLog{}, state: spec.JobState{
		ID: hex.EncodeToString(id), Status: "running", Started: time.Now().UTC().Format(time.RFC3339),
	}}
	b.mu.Lock()
	b.jobs[j.state.ID] = j
	b.keep = append(b.keep, j.state.ID)
	for len(b.keep) > 200 { // forget the oldest finished jobs
		delete(b.jobs, b.keep[0])
		b.keep = b.keep[1:]
	}
	b.mu.Unlock()

	go func() {
		var location string
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			location, err = fn(j.log)
		}()
		j.mu.Lock()
		defer j.mu.Unlock()
		j.state.Finished = time.Now().UTC().Format(time.RFC3339)
		j.state.Location = location
		if err != nil {
			j.log.Printf("✗ %v", err)
			j.state.Status, j.state.Error = "error", err.Error()
			return
		}
		j.log.Printf("✓ Finished")
		j.state.Status = "done"
	}()
	return j.snapshot()
}

func (j *job) snapshot() spec.JobState {
	j.mu.Lock()
	st := j.state
	j.mu.Unlock()
	st.Log = j.log.snapshot()
	return st
}

func (b *jobBook) get(id string) (spec.JobState, bool) {
	b.mu.Lock()
	j, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok {
		return spec.JobState{}, false
	}
	return j.snapshot(), true
}
