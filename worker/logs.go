package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"devops-allinone/shared/store"
)

const (
	logFlushInterval = 500 * time.Millisecond
	maxLogLineBytes  = 16 << 10
	maxStepLogBytes  = 5 << 20
)

// stepLog buffers a step's output and writes it to Postgres in batches.
type stepLog struct {
	store    *store.Store
	buildID  int64
	position int

	mu        sync.Mutex
	pending   []store.LogLine
	size      int
	truncated bool
	writers   []*lineWriter

	stop    chan struct{}
	stopped chan struct{}
}

func newStepLog(st *store.Store, buildID int64, position int) *stepLog {
	l := &stepLog{
		store:    st,
		buildID:  buildID,
		position: position,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go l.flushLoop()
	return l
}

func (l *stepLog) Writer(stream string) io.Writer {
	w := &lineWriter{log: l, stream: stream}
	l.mu.Lock()
	l.writers = append(l.writers, w)
	l.mu.Unlock()
	return w
}

func (l *stepLog) Systemf(format string, args ...any) {
	l.add(store.StreamSystem, fmt.Sprintf(format, args...))
}

func (l *stepLog) add(stream, line string) {
	line = strings.ReplaceAll(strings.ToValidUTF8(strings.TrimSuffix(line, "\r"), "\uFFFD"), "\x00", "")

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return
	}
	if l.size+len(line) > maxStepLogBytes {
		l.truncated = true
		line, stream = "log truncated: step output exceeded 5 MB", store.StreamSystem
	}
	l.size += len(line)
	l.pending = append(l.pending, store.LogLine{
		Position:  l.position,
		Stream:    stream,
		Line:      line,
		CreatedAt: time.Now().UTC(),
	})
}

func (l *stepLog) flushLoop() {
	defer close(l.stopped)
	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.flush()
		}
	}
}

func (l *stepLog) flush() {
	l.mu.Lock()
	lines := l.pending
	l.pending = nil
	l.mu.Unlock()

	if err := l.store.AppendLogs(detached, l.buildID, lines); err != nil {
		log.Printf("build=%d step %d: write %d log lines: %v", l.buildID, l.position, len(lines), err)
	}
}

// Close writes any partial final lines and everything still buffered.
func (l *stepLog) Close() {
	l.mu.Lock()
	writers := l.writers
	l.mu.Unlock()
	for _, w := range writers {
		w.flushPartial()
	}
	close(l.stop)
	<-l.stopped
	l.flush()
}

type lineWriter struct {
	log    *stepLog
	stream string

	mu  sync.Mutex
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.log.add(w.stream, string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLogLineBytes {
		w.log.add(w.stream, string(w.buf[:maxLogLineBytes]))
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) flushPartial() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.log.add(w.stream, string(w.buf))
		w.buf = nil
	}
}
