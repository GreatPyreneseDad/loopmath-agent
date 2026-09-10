package findings

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Emitter fans a finding out to every configured sink.
type Emitter struct {
	mu     sync.Mutex
	ring   []Finding
	ringN  int
	file   *os.File
	sink   string
	token  string
	batch  []Finding
	client *http.Client
	stop   chan struct{}
	wg     sync.WaitGroup
	total  int
}

func NewEmitter(filePath, sinkURL, sinkToken string, flush time.Duration, ringN int) (*Emitter, error) {
	e := &Emitter{ringN: ringN, sink: sinkURL, token: sinkToken, client: &http.Client{Timeout: 15 * time.Second}, stop: make(chan struct{})}
	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		e.file = f
	}
	if sinkURL != "" {
		e.wg.Add(1)
		go e.flusher(flush)
	}
	return e, nil
}

func (e *Emitter) Emit(f Finding) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.total++
	e.ring = append(e.ring, f)
	if len(e.ring) > e.ringN {
		e.ring = e.ring[len(e.ring)-e.ringN:]
	}
	if e.file != nil {
		b, _ := json.Marshal(f)
		e.file.Write(append(b, '\n'))
	}
	if e.sink != "" {
		e.batch = append(e.batch, f)
	}
}

// Recent returns the newest findings, newest first.
func (e *Emitter) Recent(n int) []Finding {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Finding, 0, n)
	for i := len(e.ring) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, e.ring[i])
	}
	return out
}

func (e *Emitter) Total() int { e.mu.Lock(); defer e.mu.Unlock(); return e.total }

func (e *Emitter) flusher(every time.Duration) {
	defer e.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.flush()
		case <-e.stop:
			e.flush()
			return
		}
	}
}

func (e *Emitter) flush() {
	e.mu.Lock()
	if len(e.batch) == 0 {
		e.mu.Unlock()
		return
	}
	b := e.batch
	e.batch = nil
	e.mu.Unlock()

	body, _ := json.Marshal(map[string]any{"schema": SchemaVersion, "findings": b})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.sink, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		log.Printf("sink: %v (requeueing %d)", err, len(b))
		e.mu.Lock()
		e.batch = append(b, e.batch...)
		if len(e.batch) > 10000 {
			e.batch = e.batch[len(e.batch)-10000:]
		}
		e.mu.Unlock()
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("sink: HTTP %d (dropping %d)", resp.StatusCode, len(b))
	}
}

func (e *Emitter) Close() {
	if e.sink != "" {
		close(e.stop)
		e.wg.Wait()
	}
	if e.file != nil {
		e.file.Close()
	}
}
