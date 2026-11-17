package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if e := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func setup(t *testing.T) (*registration, string, string) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	bin := filepath.Join(d, "bin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	script(t, bin, "gh", `echo "$GH_TOKEN" > "$HOME/auth"; if [ -f "$HOME/ghfail" ]; then exit 1; fi; echo "$3 $4" >> "$HOME/requests"; if [ "$3" = graphql ]; then cat "$HOME/graphql.json"; if [ -f "$HOME/gqlfail" ]; then exit 1; fi; exit 0; fi; case "$4" in
 */pulls/1) if [ -f "$HOME/pull.json" ]; then cat "$HOME/pull.json"; else echo '[{"state":"open","head":{"sha":"abc"}}]'; fi ;;
 */reviews*) echo '[[{"id":1,"user":{"login":"bobzoller","type":"User"},"state":"COMMENTED","submitted_at":"2026-01-01","body":""},{"id":2,"user":{"login":"bobzoller","type":"User"},"state":"PENDING","submitted_at":null,"body":"draft"},{"id":3,"user":{"login":"other","type":"User"},"state":"CHANGES_REQUESTED","submitted_at":"2026-01-01","body":"x"}]]' ;;
 */issues/1/comments*) echo '[[{"id":4,"user":{"login":"greptile-apps[bot]","type":"Bot"},"body":"finding","updated_at":"2026-01-01","html_url":"https://example/4"},{"id":5,"user":{"login":"bobzoller","type":"User"},"body":"agent reply","updated_at":"2026-01-01"}]]' ;;
 */pulls/1/comments*) echo '[[]]' ;;
 */check-runs*) if [ -f "$HOME/rerun" ]; then echo '[{"check_runs":[{"id":9,"name":"test","status":"completed","conclusion":"failure","completed_at":"2026-01-02"}]}]'; else echo '[{"check_runs":[{"id":6,"name":"test","status":"completed","conclusion":"failure","completed_at":"2026-01-01"}]}]'; fi ;;
 */statuses*) echo '[[{"id":8,"context":"deploy","state":"success"},{"id":7,"context":"deploy","state":"failure"}]]' ;;
 *) exit 1 ;; esac`)
	codex := script(t, bin, "codex", `echo "$*" >> "$HOME/queued"; if [ -f "$HOME/queuefail" ]; then exit 1; fi; if [ -f "$HOME/fail" ]; then echo "Warning: message was queued, but could not resume thread thread-1: thread already has an active or pending turn" >&2; fi`)
	return &registration{"https://github.com/acme/repo/pull/1", "thread-1"}, d, codex
}
func TestScanDurableAndRetry(t *testing.T) {
	r, d, c := setup(t)
	t.Setenv("GITHUB_TOKEN", "preferred")
	t.Setenv("GH_TOKEN", "other")
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(d, "queuefail"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = scan(db, *r, c); e == nil {
		t.Fatal("expected failed queue command to remain retryable")
	}
	os.Remove(filepath.Join(d, "queuefail"))
	if e = os.WriteFile(filepath.Join(d, "fail"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = scan(db, *r, c); e != nil {
		t.Fatalf("queued message with resume warning must be recorded: %v", e)
	}
	os.Remove(filepath.Join(d, "fail"))
	if e = scan(db, *r, c); e != nil {
		t.Fatal(e)
	}
	db.Close()
	db, e = dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = scan(db, *r, c); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "queued"))
	s := string(b)
	if strings.Count(s, "PR monitor:") != 2 {
		t.Fatalf("expected retry only after failed queue command: %s", s)
	}
	var inbox bytes.Buffer
	if e = showInbox(db, r.URL, r.Thread, &inbox); e != nil {
		t.Fatal(e)
	}
	details := inbox.String()
	for _, want := range []string{"review 1", "Bot feedback", "check"} {
		if !strings.Contains(details, want) {
			t.Fatalf("missing %s: %s", want, details)
		}
	}
	for _, bad := range []string{"review 2", "review 3", "agent reply", "deploy"} {
		if strings.Contains(details, bad) {
			t.Fatalf("unexpected %s: %s", bad, details)
		}
	}
	auth, _ := os.ReadFile(filepath.Join(d, "auth"))
	if strings.TrimSpace(string(auth)) != "preferred" {
		t.Fatal("GITHUB_TOKEN did not take precedence")
	}
	if e = os.WriteFile(filepath.Join(d, "rerun"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(d, "ghfail"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = scan(db, *r, c); e == nil {
		t.Fatal("expected API failure")
	}
	os.Remove(filepath.Join(d, "ghfail"))
	t.Setenv("GITHUB_TOKEN", "")
	if e = scan(db, *r, c); e != nil {
		t.Fatal(e)
	}
	queued, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(queued), "PR monitor:") != 2 {
		t.Fatal("new event should coalesce behind outstanding wake")
	}
	inbox.Reset()
	if e = showInbox(db, r.URL, r.Thread, &inbox); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(inbox.String(), "run 9") {
		t.Fatal("rerun missing from inbox")
	}
	auth, _ = os.ReadFile(filepath.Join(d, "auth"))
	if strings.TrimSpace(string(auth)) != "other" {
		t.Fatal("gh auth environment was not preserved")
	}
	if _, e = db.Exec("DELETE FROM prs"); e != nil {
		t.Fatal(e)
	}
	if active(db, *r) {
		t.Fatal("removed PR active")
	}
}
func TestQueueResumeWarningIsReportedWithoutRequeue(t *testing.T) {
	r, d, codex := setup(t)
	if err := os.WriteFile(filepath.Join(d, "fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	if err := queue(codex, *r, "feedback"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "message queued, but Codex could not resume") {
		t.Fatalf("warning not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), r.Thread) {
		t.Fatal("warning did not identify owning thread")
	}
}

func TestTerminalStateNotification(t *testing.T) {
	r, d, codex := setup(t)
	db, err := dbOpen(d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	register := func() {
		t.Helper()
		if _, err := db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); err != nil {
			t.Fatal(err)
		}
	}
	setPull := func(data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d, "pull.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	register()
	setPull(`[{"state":"closed","closed_at":"2026-01-01T10:00:00Z","merged_at":null}]`)
	if err = os.WriteFile(filepath.Join(d, "queuefail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = scan(db, *r, codex); err == nil || !active(db, *r) {
		t.Fatal("failed queue must retain registration for retry")
	}
	os.Remove(filepath.Join(d, "queuefail"))
	if err = os.WriteFile(filepath.Join(d, "fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = scan(db, *r, codex); err != nil || active(db, *r) {
		t.Fatalf("queued warning should remove registration: %v", err)
	}
	os.Remove(filepath.Join(d, "fail"))
	db.Close()
	db, err = dbOpen(d)
	if err != nil {
		t.Fatal(err)
	}
	register() // Previously notified terminal registrations are silently cleaned up.
	if err = scan(db, *r, codex); err != nil || active(db, *r) {
		t.Fatalf("already notified registration not removed: %v", err)
	}
	register()
	setPull(`[{"state":"closed","closed_at":"2026-01-03T10:00:00Z","merged_at":"2026-01-03T10:00:00Z"}]`)
	if err = scan(db, *r, codex); err != nil || active(db, *r) {
		t.Fatalf("merged registration not removed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(d, "queued"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if strings.Count(out, "PR monitor:") != 2 || !strings.Contains(out, "closed; monitoring removed") {
		t.Fatalf("unexpected terminal wakes: %s", out)
	}
	var inbox bytes.Buffer
	if err = showInbox(db, r.URL, r.Thread, &inbox); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inbox.String(), "PR merged at") || !strings.Contains(inbox.String(), "Monitoring removed") {
		t.Fatalf("missing merged event: %s", inbox.String())
	}
	register()
	setPull(`[{"state":"closed","closed_at":null,"merged_at":null}]`)
	if err = scan(db, *r, codex); err == nil || !active(db, *r) {
		t.Fatal("missing timestamp must retain registration")
	}
}

func TestTerminalQueuePreservesNewOwner(t *testing.T) {
	r, d, _ := setup(t)
	db, err := dbOpen(d)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(d, "pull.json"), []byte(`[{"state":"closed","closed_at":"2026-01-01T10:00:00Z"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	codex := script(t, d, "switch-owner", `sqlite3 "$HOME/state.db" "UPDATE prs SET thread='new-owner'"`)
	if err = scan(db, *r, codex); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err = db.QueryRow("SELECT thread FROM prs WHERE url=?", r.URL).Scan(&owner); err != nil || owner != "new-owner" {
		t.Fatalf("new owner lost: %q %v", owner, err)
	}
	replacement := registration{URL: r.URL, Thread: "new-owner"}
	if err = scan(db, replacement, codex); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=?", r.URL, replacement.Thread).Scan(&count); err != nil || count != 1 {
		t.Fatalf("new owner missed event: %d %v", count, err)
	}
}

func TestStartStop(t *testing.T) {
	_, d, _ := setup(t)
	exe := filepath.Join(d, "monitor")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", out, e)
	}
	run := func(arg string) {
		t.Helper()
		cmd := exec.Command(exe, arg)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%s: %s %v", arg, out, e)
		}
	}
	run("start")
	run("start")
	pid := filepath.Join(d, ".config", "codex-pr-monitor", "pid")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(pid); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	run("stop")
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(pid); os.IsNotExist(e) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
	run("stop")
}
