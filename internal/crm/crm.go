// Package crm - граница с CRM: заявка, заметка по сделке, задача менеджеру.
// Фейк держит записи в памяти и печатает их, чтобы демо было видно без
// внешнего аккаунта.
package crm

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// CRM - то, что квалификатор делает в CRM. Key - ключ идемпотентности:
// повтор с тем же ключом не создаёт вторую запись.
type CRM interface {
	CreateLead(ctx context.Context, name string) (string, error)
	AddNote(ctx context.Context, key, leadRef, text string) error
	AddTask(ctx context.Context, key, leadRef, managerID, text string) error
}

// Record - одна запись фейка: что и с какими полями вызвали.
type Record struct {
	Kind      string // lead, note, task
	Key       string
	LeadRef   string
	ManagerID string
	Text      string
}

// Fake - CRM в памяти. Out - куда печатать записи; nil - молча.
type Fake struct {
	Out io.Writer

	mu      sync.Mutex
	records []Record
	keys    map[string]bool
	leads   int
}

// NewFake создаёт пустую CRM.
func NewFake(out io.Writer) *Fake {
	return &Fake{Out: out, keys: map[string]bool{}}
}

// CreateLead заводит заявку и возвращает её номер.
func (f *Fake) CreateLead(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads++
	ref := fmt.Sprintf("lead-%d", f.leads)
	f.add(Record{Kind: "lead", Key: ref, LeadRef: ref, Text: name})
	return ref, nil
}

// AddNote пишет заметку в сделку.
func (f *Fake) AddNote(_ context.Context, key, leadRef, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[key] {
		return nil
	}
	f.add(Record{Kind: "note", Key: key, LeadRef: leadRef, Text: text})
	return nil
}

// AddTask ставит задачу менеджеру; пустой managerID - ответственному сделки.
func (f *Fake) AddTask(_ context.Context, key, leadRef, managerID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[key] {
		return nil
	}
	f.add(Record{Kind: "task", Key: key, LeadRef: leadRef, ManagerID: managerID, Text: text})
	return nil
}

// Records - копия всех записей в порядке создания.
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
	// Печать в демо - часть вывода, а не лог: её ошибка ничего не ломает.
	_, _ = fmt.Fprintf(f.Out, "[CRM %s %s] %s\n", record.Kind, target, record.Text)
}
