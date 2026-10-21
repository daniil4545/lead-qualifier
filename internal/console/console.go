// Package console - the demo transport: the lead writes to stdin, the bot answers on stdout.
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

// Console prints the bot messages of its lead. Writes are under a mutex: the bot and the CRM
// write from the delivery loop, the input prompt from the read loop.
type Console struct {
	out   io.Writer
	mu    sync.Mutex
	count int
	peer  string
}

// errForeignPeer - a message to a lead of a previous session: it does not go to this terminal.
var errForeignPeer = errors.New("peer is not in this console session")

// New creates a transport on top of out.
func New(out io.Writer) *Console {
	return &Console{out: out}
}

// Send prints a bot message and returns its number in the session.
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

// Write - printing of the fake CRM through the same mutex.
func (c *Console) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(data)
}

// idleTimeout - how long the demo waits for the last reply after the input ends.
const idleTimeout = 3 * time.Minute

// Run drives the demo: asks for the lead's name, creates a request in the CRM and a case, then
// each stdin line is a lead message. The end of input ends the demo once the bot
// has answered the last message.
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

// newPeerRef - the lead's address in the session: each demo is a new lead.
func newPeerRef() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate peer ref: %w", err)
	}
	return "console:" + hex.EncodeToString(raw[:]), nil
}
