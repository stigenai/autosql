// Package auditlog provides an append-only, hash-linked control-plane audit log
// and bounded notification delivery.
package auditlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid       = errors.New("invalid audit record")
	ErrTampered      = errors.New("tampered audit log")
	ErrSensitiveData = errors.New("sensitive data in audit record")
)

type Result string

const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
	ResultRefused Result = "refused"
)

// Event is the data recorded for every privileged mutation or observation.
// Detail must contain metadata only; secrets and query results are rejected.
type Event struct {
	ID            string            `json:"id"`
	At            time.Time         `json:"at"`
	Actor         string            `json:"actor"`
	Action        string            `json:"action"`
	Subject       string            `json:"subject"`
	Result        Result            `json:"result"`
	CorrelationID string            `json:"correlation_id"`
	Detail        map[string]string `json:"detail,omitempty"`
}

type Record struct {
	Sequence     uint64 `json:"sequence"`
	PreviousHash string `json:"previous_hash,omitempty"`
	Hash         string `json:"hash"`
	Event        Event  `json:"event"`
}

func hashRecord(r Record) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	s := sha256.Sum256(append([]byte("autosql.audit/v1\x00"), b...))
	return hex.EncodeToString(s[:])
}

func validateEvent(e Event) error {
	if e.ID == "" || e.Actor == "" || e.Action == "" || e.Subject == "" || e.CorrelationID == "" || e.At.IsZero() {
		return ErrInvalid
	}
	if e.Result != ResultSuccess && e.Result != ResultFailure && e.Result != ResultRefused {
		return ErrInvalid
	}
	for k, v := range e.Detail {
		text := strings.ToLower(k + "=" + v)
		for _, marker := range []string{"password", "secret", "token", "private_key", "query_result", "credential"} {
			if strings.Contains(text, marker) {
				return fmt.Errorf("%w: %s", ErrSensitiveData, k)
			}
		}
	}
	return nil
}

type Store struct {
	mu      sync.RWMutex
	records []Record
}

func New() *Store { return &Store{} }

func (s *Store) Append(e Event) (Record, error) {
	if err := validateEvent(e); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.Event.ID == e.ID {
			if reflect.DeepEqual(r.Event, e) {
				return r, nil
			}
			return Record{}, fmt.Errorf("%w: duplicate event id", ErrInvalid)
		}
	}
	var previous string
	if len(s.records) > 0 {
		previous = s.records[len(s.records)-1].Hash
	}
	r := Record{Sequence: uint64(len(s.records) + 1), PreviousHash: previous, Event: e}
	r.Hash = hashRecord(r)
	s.records = append(s.records, r)
	return r, nil
}

func (s *Store) Verify() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var previous string
	for i, r := range s.records {
		if r.Sequence != uint64(i+1) || r.PreviousHash != previous || r.Hash != hashRecord(r) || validateEvent(r.Event) != nil {
			return ErrTampered
		}
		previous = r.Hash
	}
	return nil
}

type Filter struct {
	Actor, Action, Subject, CorrelationID string
	Result                                Result
	Since, Until                          time.Time
}

func (s *Store) Search(f Filter) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0)
	for _, r := range s.records {
		e := r.Event
		if f.Actor != "" && e.Actor != f.Actor || f.Action != "" && e.Action != f.Action || f.Subject != "" && e.Subject != f.Subject || f.CorrelationID != "" && e.CorrelationID != f.CorrelationID || f.Result != "" && e.Result != f.Result || !f.Since.IsZero() && e.At.Before(f.Since) || !f.Until.IsZero() && e.At.After(f.Until) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Export returns canonical JSONL suitable for compliance evidence or backup.
func (s *Store) Export(f Filter) ([]byte, error) {
	rows := s.Search(f)
	var b strings.Builder
	for _, r := range rows {
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// Retain removes records older than before and establishes a verifiable
// retained log beginning at sequence one. Callers should export first.
func (s *Store) Retain(before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []Event
	for _, r := range s.records {
		if !r.Event.At.Before(before) {
			kept = append(kept, r.Event)
		}
	}
	s.records = nil
	for _, e := range kept {
		var previous string
		if len(s.records) > 0 {
			previous = s.records[len(s.records)-1].Hash
		}
		r := Record{Sequence: uint64(len(s.records) + 1), PreviousHash: previous, Event: e}
		r.Hash = hashRecord(r)
		s.records = append(s.records, r)
	}
	return nil
}

type Notification struct {
	Channel string
	Record  Record
}
type Handler func(Notification) error

// Router retries delivery and deduplicates by record hash and channel.
type Router struct {
	mu      sync.Mutex
	deliver map[string]bool
	Handler Handler
	Retries int
}

func NewRouter(h Handler) *Router { return &Router{Handler: h, deliver: map[string]bool{}, Retries: 3} }

func (r *Router) Deliver(n Notification) error {
	if r == nil || r.Handler == nil || n.Channel == "" || n.Record.Hash == "" {
		return ErrInvalid
	}
	key := n.Channel + "\x00" + n.Record.Hash
	r.mu.Lock()
	if r.deliver[key] {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	attempts := r.Retries
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = r.Handler(n); err == nil {
			r.mu.Lock()
			r.deliver[key] = true
			r.mu.Unlock()
			return nil
		}
	}
	return err
}
