// Package crm - the CRM boundary: a lead request, a deal note, a manager task.
// The fake keeps records in memory and prints them so the demo is visible without
// an external account.
package crm

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// CRM - what the qualifier does in the CRM. Key is the idempotency key:
// a repeat with the same key does not create a second record.
type CRM interface {
	CreateLead(ctx context.Context, name string) (string, error)
	AddNote(ctx context.Context, key, leadRef, text string) error
	AddTask(ctx context.Context, key, leadRef, managerID, text string) error
}

// Record - one fake record: what was called and with which fields.
type Record struct {
	Kind      string // lead, note, task
	Key       string
	LeadRef   string
	ManagerID string
	Text      string
}

// Fake - an in-memory CRM. Out is where to print records; nil means silently.
type Fake struct {
	Out io.Writer

	mu      sync.Mutex
	records []Record
	keys    map[string]bool
	leads   int
}

// NewFake creates an empty CRM.
func NewFake(out io.Writer) *Fake {
	return &Fake{Out: out, keys: map[string]bool{}}
}

// CreateLead creates a request and returns its number.
func (f *Fake) CreateLead(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads++
	ref := fmt.Sprintf("lead-%d", f.leads)
	f.add(Record{Kind: "lead", Key: ref, LeadRef: ref, Text: name})
	return ref, nil
}

// AddNote writes a note to the deal.
func (f *Fake) AddNote(_ context.Context, key, leadRef, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[key] {
		return nil
	}
	f.add(Record{Kind: "note", Key: key, LeadRef: leadRef, Text: text})
	return nil
}

// AddTask puts a task for the manager; an empty managerID means the deal owner.
func (f *Fake) AddTask(_ context.Context, key, leadRef, managerID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[key] {
		return nil
	}
	f.add(Record{Kind: "task", Key: key, LeadRef: leadRef, ManagerID: managerID, Text: text})
	return nil
}

// Records - a copy of all records in creation order.
func (f *Fake) Records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.records...)
}

func (f *Fake) add(record Record) {
	f.keys[record.Key] = true
	f.records = append(f.records, record)
	if f.Out == nil {
		return
	}
	target := record.LeadRef
	if record.ManagerID != "" {
		target += ", менеджер " + record.ManagerID
	}
	// Printing in the demo is part of the output, not a log: its error breaks nothing.
	_, _ = fmt.Fprintf(f.Out, "[CRM %s %s] %s\n", record.Kind, target, record.Text)
}
