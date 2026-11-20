package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMultiOwnerFanoutAndLateJoin(t *testing.T) {
	db, r, _, c := registeredInbox(t)
	second := registration{r.URL, "thread-2"}
	if err := addOwner(db, second); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	keys := []string{"review:900:" + past, "review:901:" + future}
	if err := saveEvents(db, *r, keys, []string{"old", "new"}); err != nil {
		t.Fatal(err)
	}
	if err := wakeOwners(db, c, r.URL, ""); err != nil {
		t.Fatal(err)
	}
	var first, late int
	if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, second.Thread).Scan(&late); err != nil {
		t.Fatal(err)
	}
	if first != 2 || late != 1 {
		t.Fatalf("wrong fanout %d %d", first, late)
	}
	w, n := ids(t, db, *r)
	if err := ackInbox(db, r.URL, r.Thread, w, n); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, second.Thread).Scan(&count); err != nil || count != 1 {
		t.Fatal("ack affected second owner", count, err)
	}
	if err := saveEvents(db, *r, keys, []string{"old", "new"}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&count); err != nil || count != 0 {
		t.Fatal("duplicate event", count, err)
	}
	if err := removeOwners(db, r.URL, "", false); err == nil {
		t.Fatal("ambiguous removal accepted")
	}
	if err := removeOwners(db, r.URL, second.Thread, false); err != nil {
		t.Fatal(err)
	}
	if !active(db, *r) || active(db, second) {
		t.Fatal("wrong owner removed")
	}
	if err := removeOwners(db, r.URL, "", true); err != nil {
		t.Fatal(err)
	}
}

func TestSharedPollQueriesOnce(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "rest"}[fallback], func(t *testing.T) {
			db, r, d, c := registeredInbox(t)
			if _, err := db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, "thread-2"); err != nil {
				t.Fatal(err)
			}
			if fallback {
				writeGraph(t, d, `{"errors":[{"message":"Cannot query field fullDatabaseId"}]}`)
			} else {
				writeGraph(t, d, `{"data":{"p0":{"pullRequest":`+openGraph+`}}}`)
			}
			poll(db, c)
			b, _ := os.ReadFile(filepath.Join(d, "requests"))
			s := string(b)
			if strings.Count(s, "graphql") != 1 || strings.Count(s, "check-runs") != 1 || strings.Count(s, "statuses") != 1 {
				t.Fatalf("duplicate calls: %s", s)
			}
			if fallback && strings.Count(s, "/reviews?") != 1 {
				t.Fatalf("REST not shared: %s", s)
			}
			for _, thread := range []string{r.Thread, "thread-2"} {
				var n int
				if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, thread).Scan(&n); err != nil || n == 0 {
					t.Fatalf("missing inbox %s %d %v", thread, n, err)
				}
			}
		})
	}
}

func TestTerminalFailureIsIsolatedAndRetried(t *testing.T) {
	db, r, d, _ := registeredInbox(t)
	second := registration{r.URL, "thread-2"}
	if _, err := db.Exec("INSERT INTO prs VALUES(?,?)", second.URL, second.Thread); err != nil {
		t.Fatal(err)
	}
	c := script(t, filepath.Join(d, "bin"), "selective-codex", `echo "$*" >> "$HOME/terminal-queued"; case "$*" in *thread-2*) [ -f "$HOME/allow" ] || exit 1 ;; esac`)
	writeGraph(t, d, `{"data":{"p0":{"pullRequest":`+closedGraph+`}}}`)
	poll(db, c)
	if active(db, *r) || !active(db, second) {
		t.Fatal("failed owner was not isolated")
	}
	if err := os.WriteFile(filepath.Join(d, "allow"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	poll(db, c)
	if active(db, second) {
		t.Fatal("failed owner was not retried")
	}
	for _, thread := range []string{r.Thread, second.Thread} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=? AND event LIKE 'terminal:%'", r.URL, thread).Scan(&n); err != nil || n != 1 {
			t.Fatalf("missing terminal %s %d %v", thread, n, err)
		}
	}
}

func TestRemovalSerializesAgainstWake(t *testing.T) {
	db, r, d, _ := registeredInbox(t)
	if err := saveEvents(db, *r, []string{"review:90:2026-11-20T00:00:00Z"}, []string{"test"}); err != nil {
		t.Fatal(err)
	}
	other, err := dbOpen(d)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	c := script(t, filepath.Join(d, "bin"), "slow-codex", `touch "$HOME/queue-started"; while [ ! -f "$HOME/queue-release" ]; do sleep 0.01; done`)
	wakeDone := make(chan error, 1)
	go func() { _, err := sendWake(db, c, *r); wakeDone <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(d, "queue-started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	removeDone := make(chan error, 1)
	go func() { removeDone <- removeOwners(other, r.URL, r.Thread, false) }()
	select {
	case err := <-removeDone:
		t.Fatalf("remove completed while wake was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(d, "queue-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-wakeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if active(db, *r) {
		t.Fatal("owner remains after removal")
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&n); err != nil || n != 0 {
		t.Fatal("removed inbox survived", n, err)
	}
}

func TestLateOwnerReceivesNewFailedStatus(t *testing.T) {
	db, r, d, c := registeredInbox(t)
	second := registration{r.URL, "thread-2"}
	if err := addOwner(db, second); err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	writeGraph(t, d, `{"data":{"p0":{"pullRequest":`+openGraph+`}}}`)
	if err := os.WriteFile(filepath.Join(d, "status.json"), []byte(`[[{"id":7,"context":"deploy","state":"failure","updated_at":"`+when+`"}]]`), 0600); err != nil {
		t.Fatal(err)
	}
	script(t, filepath.Join(d, "bin"), "gh", `case "$3 $4" in
 "graphql "*) cat "$HOME/graphql.json" ;;
 *check-runs*) echo '[{"check_runs":[]}]' ;;
 *statuses*) cat "$HOME/status.json" ;;
 *) exit 1 ;; esac`)
	poll(db, c)
	for _, thread := range []string{r.Thread, second.Thread} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=? AND event=?", r.URL, thread, "status:abc:7:failure").Scan(&n); err != nil || n != 1 {
			t.Fatalf("status missing for %s: %d %v", thread, n, err)
		}
	}
	if err := removeOwners(db, r.URL, "", false); err == nil {
		t.Fatal("missing selector accepted")
	}
	if err := removeOwners(db, r.URL, second.Thread, false); err != nil {
		t.Fatal(err)
	}
	if err := removeOwners(db, r.URL, "", false); err == nil {
		t.Fatal("missing selector accepted for sole owner")
	}
}

func TestLegacyMigrationPreservesPendingTransfer(t *testing.T) {
	d := t.TempDir()
	old, err := sql.Open("sqlite", filepath.Join(d, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE prs(url TEXT PRIMARY KEY, thread TEXT NOT NULL); CREATE TABLE seen(url TEXT NOT NULL,event TEXT NOT NULL,PRIMARY KEY(url,event)); CREATE TABLE inbox_items(id INTEGER PRIMARY KEY AUTOINCREMENT,url TEXT NOT NULL,thread TEXT NOT NULL,event TEXT NOT NULL,detail TEXT NOT NULL,UNIQUE(url,thread,event)); INSERT INTO prs VALUES('https://github.com/a/b/pull/1','new'); INSERT INTO seen VALUES('https://github.com/a/b/pull/1','review:1:x'); INSERT INTO inbox_items(url,thread,event,detail) VALUES('https://github.com/a/b/pull/1','old','review:1:x','feedback');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbOpen(d)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE event='review:1:x'").Scan(&n); err != nil || n != 2 {
		t.Fatalf("pending feedback lost: %d %v", n, err)
	}
	if err = addOwner(db, registration{"https://github.com/a/b/pull/1", "third"}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE thread='third'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("late owner got history: %d %v", n, err)
	}
}
