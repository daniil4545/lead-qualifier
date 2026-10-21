package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// syncBuffer - the demo output read by the test while the demo still writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// tempDatabase creates an empty database next to DATABASE_URL and returns its URL.
func tempDatabase(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration tier: needs Postgres")
	}
	adminURL := os.Getenv("DATABASE_URL")
	if adminURL == "" {
		t.Fatal("DATABASE_URL is required for the integration tier; run with -short to skip it")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	name := fmt.Sprintf("lq_demo_%d", os.Getpid())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// waitFor polls the output until it holds want at least count times.
func waitFor(t *testing.T, out *syncBuffer, want string, count int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(out.String(), want) < count {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d x %q in output:\n%s", count, want, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The offline demo: the whole pipeline (store, agent loop, guard, critic, gate, calendar,
// outbox, fake CRM) runs on the scripted model stub from a lead's name to a booked meeting.
// Not parallel: the model address and the database go through the environment.
func TestRunDemo_OfflineBooksMeeting(t *testing.T) {
	t.Setenv("DATABASE_URL", tempDatabase(t))
	t.Setenv("CATALOG_DIR", "../../data/example")
	reader, writer := io.Pipe()
	out := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDemo(ctx, reader, out, true) }()

	say := func(line string, botReplies int) {
		t.Helper()
		if _, err := io.WriteString(writer, line+"\n"); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
		waitFor(t, out, "Бот:", botReplies)
	}
	waitFor(t, out, "Имя лида:", 1)
	say("Олег", 1)
	say("А сколько времени занимает подключение?", 2)
	say("У нас сеть из четырёх магазинов стройматериалов в Бердске, выручка около 12 млн в месяц. "+
		"Я генеральный директор и владелец.", 3)
	say("Давайте в 11", 4)
	waitFor(t, out, "[CRM task", 1)
	if err := writer.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("runDemo: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"подключение занимает от одной до двух недель",
		"Вот ближайшие свободные окна для встречи с менеджером:",
		"Записала вас на встречу",
		"[CRM note lead-1]",
		"направление: розничная торговля",
	} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
