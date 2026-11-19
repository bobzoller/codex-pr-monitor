package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func registeredInbox(t *testing.T) (*sql.DB, *registration, string, string) {
	t.Helper()
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db, r, d, c
}
func ids(t *testing.T, db *sql.DB, r registration) (int64, int64) {
	t.Helper()
	var wake, through int64
	if e := db.QueryRow("SELECT id FROM inbox_wakes WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&wake); e != nil {
		t.Fatal(e)
	}
	if e := db.QueryRow("SELECT max(id) FROM inbox_items WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&through); e != nil {
		t.Fatal(e)
	}
	return wake, through
}
func TestCoalescesEventsUntilSnapshotAcknowledged(t *testing.T) {
	db, r, d, c := registeredInbox(t)
	if e := saveEvents(db, *r, []string{"a"}, []string{"first"}); e != nil {
		t.Fatal(e)
	}
	if _, e := sendWake(db, c, *r); e != nil {
		t.Fatal(e)
	}
	wake, through := ids(t, db, *r)
	for _, k := range []string{"b", "c"} {
		if e := saveEvents(db, *r, []string{k}, []string{"event " + k}); e != nil {
			t.Fatal(e)
		}
		if _, e := sendWake(db, c, *r); e != nil {
			t.Fatal(e)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(raw), "PR monitor:") != 1 {
		t.Fatalf("events produced duplicate wakes: %s", raw)
	}
	if e := ackInbox(db, r.URL, r.Thread, wake, through); e != nil {
		t.Fatal(e)
	}
	flushInbox(db, c)
	wake2, through2 := ids(t, db, *r)
	if wake2 == wake || through2 <= through {
		t.Fatal("new events not preserved after snapshot ack")
	}
	if e := ackInbox(db, r.URL, r.Thread, wake, through); e == nil {
		t.Fatal("stale ack must not clear newer wake")
	}
	var b bytes.Buffer
	if e := showInbox(db, r.URL, r.Thread, &b); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(b.String(), "first") || !strings.Contains(b.String(), "event b") || !strings.Contains(b.String(), "event c") {
		t.Fatalf("wrong pending inbox: %s", b.String())
	}
	if !strings.Contains(b.String(), fmt.Sprintf("--wake %d --through %d", wake2, through2)) {
		t.Fatal("ack command missing snapshot")
	}
	if e := ackInbox(db, r.URL, r.Thread, wake2, through2); e != nil {
		t.Fatal(e)
	}
	flushInbox(db, c)
	raw, _ = os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(raw), "PR monitor:") != 2 {
		t.Fatalf("expected exactly one follow-up wake: %s", raw)
	}
}
func TestTerminalStateCoalescesBehindExistingWake(t *testing.T) {
	db, r, d, c := registeredInbox(t)
	if e := saveEvents(db, *r, []string{"review:1:x"}, []string{"review"}); e != nil {
		t.Fatal(e)
	}
	if _, e := sendWake(db, c, *r); e != nil {
		t.Fatal(e)
	}
	wake, through := ids(t, db, *r)
	if e := os.WriteFile(filepath.Join(d, "pull.json"), []byte(`[{"state":"closed","merged_at":"2026-01-03T10:00:00Z"}]`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := scan(db, *r, c); e != nil {
		t.Fatal(e)
	}
	if active(db, *r) {
		t.Fatal("merged PR still registered")
	}
	raw, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(raw), "PR monitor:") != 1 {
		t.Fatalf("terminal change created extra wake: %s", raw)
	}
	if e := ackInbox(db, r.URL, r.Thread, wake, through); e != nil {
		t.Fatal(e)
	}
	flushInbox(db, c)
	wake2, through2 := ids(t, db, *r)
	var inbox bytes.Buffer
	if e := showInbox(db, r.URL, r.Thread, &inbox); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(inbox.String(), "PR merged") || !strings.Contains(inbox.String(), "Monitoring removed") {
		t.Fatalf("terminal event lost: %s", inbox.String())
	}
	if e := ackInbox(db, r.URL, r.Thread, wake2, through2); e != nil {
		t.Fatal(e)
	}
}
func TestInboxPersistsAndFailedQueueRetries(t *testing.T) {
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	if e = saveEvents(db, *r, []string{"first"}, []string{"first"}); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(d, "queuefail"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = sendWake(db, c, *r); e == nil {
		t.Fatal("expected queue failure")
	}
	db.Close()
	db, e = dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	os.Remove(filepath.Join(d, "queuefail"))
	flushInbox(db, c)
	wake, through := ids(t, db, *r)
	if e = ackInbox(db, r.URL, r.Thread, wake, through); e != nil {
		t.Fatal(e)
	}
}
func TestOnlyOneDelayedRecoveryRetry(t *testing.T) {
	db, r, d, c := registeredInbox(t)
	if e := saveEvents(db, *r, []string{"a"}, []string{"first"}); e != nil {
		t.Fatal(e)
	}
	if _, e := sendWake(db, c, *r); e != nil {
		t.Fatal(e)
	}
	wake, through := ids(t, db, *r)
	old := time.Now().Add(-wakeRetryAfter - time.Minute).Unix()
	if _, e := db.Exec("UPDATE inbox_wakes SET sent_at=?", old); e != nil {
		t.Fatal(e)
	}
	flushInbox(db, c)
	if _, e := db.Exec("UPDATE inbox_wakes SET sent_at=?", old); e != nil {
		t.Fatal(e)
	}
	flushInbox(db, c)
	flushInbox(db, c)
	raw, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(raw), "PR monitor:") != 2 {
		t.Fatalf("unexpected number of retries: %s", raw)
	}
	if e := ackInbox(db, r.URL, r.Thread, wake, through); e != nil {
		t.Fatal(e)
	}
}

func TestOwnershipTransferMovesPendingOnce(t *testing.T) {
	db, old, _, codex := registeredInbox(t)
	if err := saveEvents(db, *old, []string{"review:1"}, []string{"review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sendWake(db, codex, *old); err != nil {
		t.Fatal(err)
	}
	newOwner := registration{old.URL, "new-owner"}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE prs SET thread=? WHERE url=?", newOwner.Thread, old.URL); err != nil {
		t.Fatal(err)
	}
	if err = rehomeInbox(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", old.URL, old.Thread).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old pending items: %d, %v", count, err)
	}
	if _, err = sendWake(db, codex, newOwner); err != nil {
		t.Fatal(err)
	}
	wake, through := ids(t, db, newOwner)
	if err = ackInbox(db, newOwner.URL, newOwner.Thread, wake, through); err != nil {
		t.Fatal(err)
	}
	if err = saveEvents(db, newOwner, []string{"review:1"}, []string{"review"}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=?", old.URL).Scan(&count); err != nil || count != 0 {
		t.Fatalf("handled event reappeared: %d, %v", count, err)
	}
}
func TestFailedAckDoesNotEraseOtherEvents(t *testing.T) {
	db, r, _, c := registeredInbox(t)
	if e := saveEvents(db, *r, []string{"a"}, []string{"first"}); e != nil {
		t.Fatal(e)
	}
	if _, e := sendWake(db, c, *r); e != nil {
		t.Fatal(e)
	}
	wake, through := ids(t, db, *r)
	if e := ackInbox(db, r.URL, r.Thread, wake, through+1); e == nil {
		t.Fatal("invalid cursor accepted")
	}
	if e := ackInbox(db, r.URL, r.Thread, wake+1, through); e == nil {
		t.Fatal("stale wake accepted")
	}
	var n int
	if e := db.QueryRow("SELECT count(*) FROM inbox_items").Scan(&n); e != nil || n != 1 {
		t.Fatal("failed ack removed events")
	}
}
func TestInboxCLIAndExplicitRemove(t *testing.T) {
	r, d, c := setup(t)
	config, e := configDir()
	if e != nil {
		t.Fatal(e)
	}
	db, e := dbOpen(config)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	if e = saveEvents(db, *r, []string{"a"}, []string{"feedback"}); e != nil {
		t.Fatal(e)
	}
	if _, e = sendWake(db, c, *r); e != nil {
		t.Fatal(e)
	}
	wake, through := ids(t, db, *r)
	if e = run([]string{"ack", "--url", r.URL, "--thread", r.Thread, "--wake", fmt.Sprint(wake), "--through", fmt.Sprint(through)}); e != nil {
		t.Fatal(e)
	}
	if e = saveEvents(db, *r, []string{"b"}, []string{"more"}); e != nil {
		t.Fatal(e)
	}
	if e = run([]string{"remove", "--url", r.URL}); e != nil {
		t.Fatal(e)
	}
	flushInbox(db, c)
	var n int
	if e = db.QueryRow("SELECT count(*) FROM inbox_items").Scan(&n); e != nil || n != 0 {
		t.Fatalf("remove left inbox items: %d %v", n, e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(b), "PR monitor:") != 1 {
		t.Fatal("remove generated a wake")
	}
}
func TestMigrationPreservesExistingCheckpoints(t *testing.T) {
	d := t.TempDir()
	old, e := sql.Open("sqlite", filepath.Join(d, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = old.Exec(`CREATE TABLE prs(url TEXT PRIMARY KEY,thread TEXT NOT NULL); CREATE TABLE seen(url TEXT NOT NULL,event TEXT NOT NULL,PRIMARY KEY(url,event)); INSERT INTO prs VALUES('https://github.com/acme/repo/pull/1','owner'); INSERT INTO seen VALUES('https://github.com/acme/repo/pull/1','review:1:2026-01-01');`); e != nil {
		t.Fatal(e)
	}
	old.Close()
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	r := registration{"https://github.com/acme/repo/pull/1", "owner"}
	if !active(db, r) || !already(db, r.URL, "review:1:2026-01-01") {
		t.Fatal("migration lost existing state")
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM inbox_items").Scan(&n); e != nil || n != 0 {
		t.Fatalf("inbox migration failed: %d %v", n, e)
	}
}
