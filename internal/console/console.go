// Package console - транспорт демо: лид пишет в stdin, бот отвечает в stdout.
package console

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/crm"
	"github.com/daniil4545/lead-qualifier/internal/dialog"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// Console печатает сообщения бота своего лида. Запись под мьютексом: бот и CRM
// пишут из цикла доставки, приглашение ввода - из цикла чтения.
type Console struct {
	out   io.Writer
	mu    sync.Mutex
	count int
	peer  string
}

// errForeignPeer - сообщение лиду прошлой сессии: в этот терминал оно не идёт.
var errForeignPeer = errors.New("peer is not in this console session")

// New создаёт транспорт поверх out.
func New(out io.Writer) *Console {
	return &Console{out: out}
}

// Send печатает сообщение бота и возвращает его номер в сессии.
func (c *Console) Send(_ context.Context, peerRef, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if peerRef != c.peer {
		return "", errForeignPeer
	}
	c.count++
	if _, err := fmt.Fprintf(c.out, "\nБот: %s\n\n", text); err != nil {
		return "", fmt.Errorf("print bot message: %w", err)
	}
	return fmt.Sprintf("console-%d", c.count), nil
}

// Write - печать фейковой CRM через тот же мьютекс.
func (c *Console) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(data)
}

// idleTimeout - сколько демо ждёт последний ответ после конца ввода.
const idleTimeout = 3 * time.Minute

// Run ведёт демо: спрашивает имя лида, заводит заявку в CRM и кейс, дальше
// каждая строка stdin - реплика лида. Конец ввода завершает демо, когда бот
// ответил на последнюю реплику.
func Run(ctx context.Context, worker *dialog.Worker, leads crm.CRM, console *Console, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	if _, err := fmt.Fprint(console, "Имя лида: "); err != nil {
		return err
	}
	if !scanner.Scan() {
		return errors.New("lead name is required")
	}
	name := strings.TrimSpace(scanner.Text())
	if name == "" {
		return errors.New("lead name is required")
	}
	crmRef, err := leads.CreateLead(ctx, name)
	if err != nil {
		return fmt.Errorf("create crm lead: %w", err)
	}
	peerRef, err := newPeerRef()
	if err != nil {
		return err
	}
	console.mu.Lock()
	console.peer = peerRef
	console.mu.Unlock()
	if _, err := worker.StartCase(ctx, qualify.Lead{Name: name, PeerRef: peerRef, CRMRef: crmRef}); err != nil {
		return fmt.Errorf("start case: %w", err)
	}
	line := 0
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		line++
		if err := worker.Ingest(ctx, peerRef, text, fmt.Sprintf("in-%d", line)); err != nil {
			return fmt.Errorf("ingest message: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return waitIdle(ctx, worker, peerRef)
}

func waitIdle(ctx context.Context, worker *dialog.Worker, peerRef string) error {
	ctx, cancel := context.WithTimeout(ctx, idleTimeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		idle, err := worker.Idle(ctx, peerRef)
		if err != nil || idle {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// newPeerRef - адрес лида в сессии: каждое демо - новый лид.
func newPeerRef() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate peer ref: %w", err)
	}
	return "console:" + hex.EncodeToString(raw[:]), nil
}
