package router

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is one line in the usage ledger. Append-only JSONL: trivially
// greppable, survives crashes, and never needs a schema migration.
type Record struct {
	TS         time.Time `json:"ts"`
	Class      string    `json:"class"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	OK         bool      `json:"ok"`
	ErrKind    string    `json:"err_kind,omitempty"`
	Err        string    `json:"err,omitempty"`
	PromptTok  int       `json:"prompt_tok,omitempty"`
	OutTok     int       `json:"out_tok,omitempty"`
	Estimated  bool      `json:"estimated,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
	TTFTMS     int64     `json:"ttft_ms,omitempty"`
	Attempt    int       `json:"attempt,omitempty"`
	FellBackTo bool      `json:"fellback,omitempty"`
}

// Ledger appends usage records. Token accounting is the only way to know
// whether your free-tier stack is actually holding, so every call is logged
// whether it succeeded or not.
type Ledger struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
}

func NewLedger(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "usage.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Ledger{path: path, f: f, w: bufio.NewWriter(f)}, nil
}

func (l *Ledger) Append(r Record) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.w.Write(b)
	l.w.WriteByte('\n')
	l.w.Flush()
}

func (l *Ledger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Flush()
	return l.f.Close()
}

func (l *Ledger) Path() string { return l.path }

// Stat is an aggregated view over the ledger.
type Stat struct {
	Key       string
	Calls     int
	Failures  int
	PromptTok int
	OutTok    int
	TotalMS   int64
	Estimated bool
}

func (s Stat) AvgLatencyMS() int64 {
	ok := s.Calls - s.Failures
	if ok <= 0 {
		return 0
	}
	return s.TotalMS / int64(ok)
}

// Summarize reads the ledger and aggregates by the given key function,
// counting only records at or after `since`.
func Summarize(path string, since time.Time, keyOf func(Record) string) ([]Stat, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	acc := map[string]*Stat{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		k := keyOf(r)
		s, ok := acc[k]
		if !ok {
			s = &Stat{Key: k}
			acc[k] = s
		}
		s.Calls++
		if !r.OK {
			s.Failures++
		} else {
			s.TotalMS += r.LatencyMS
		}
		s.PromptTok += r.PromptTok
		s.OutTok += r.OutTok
		if r.Estimated {
			s.Estimated = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := make([]Stat, 0, len(acc))
	for _, s := range acc {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PromptTok+out[i].OutTok != out[j].PromptTok+out[j].OutTok {
			return out[i].PromptTok+out[i].OutTok > out[j].PromptTok+out[j].OutTok
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}
