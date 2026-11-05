package main

import (
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
	script(t, bin, "gh", `echo "$GH_TOKEN" > "$HOME/auth"; if [ -f "$HOME/ghfail" ]; then exit 1; fi; case "$4" in
 */pulls/1) echo '[{"state":"open","head":{"sha":"abc"}}]' ;;
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
	for _, want := range []string{"review 1", "Bot feedback", "check"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s: %s", want, s)
		}
	}
	for _, bad := range []string{"review 2", "review 3", "agent reply", "deploy"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unexpected %s: %s", bad, s)
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
	if strings.Count(string(queued), "PR monitor:") != 3 || !strings.Contains(string(queued), "run 9") {
		t.Fatal("rerun was not delivered after retry")
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
func TestWorkingHours(t *testing.T) {
	loc := time.FixedZone("local", -10*3600)
	for _, tc := range []struct {
		date string
		want bool
	}{{"2026-11-06T08:00:00-10:00", true}, {"2026-11-06T18:00:00-10:00", false}, {"2026-11-07T09:00:00-10:00", false}} {
		v, e := time.ParseInLocation(time.RFC3339, tc.date, loc)
		if e != nil || working(v) != tc.want {
			t.Fatal(tc)
		}
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
