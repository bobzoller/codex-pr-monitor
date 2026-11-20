package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var prPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/([1-9][0-9]*)/?$`)
var resumeWarning = regexp.MustCompile(`(?im)^warning: message was queued, but could not resume thread\b`)

type registration struct{ URL, Thread string }
type actor struct{ Login, Type string }

// Use explicit tags for fields whose API names contain underscores.
type apiComment struct {
	ID        int64  `json:"id"`
	User      actor  `json:"user"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
	ReviewID  int64  `json:"pull_request_review_id"`
}
type apiReview struct {
	ID          int64   `json:"id"`
	User        actor   `json:"user"`
	State       string  `json:"state"`
	SubmittedAt *string `json:"submitted_at"`
	Body        string  `json:"body"`
}
type pull struct {
	State    string `json:"state"`
	ClosedAt string `json:"closed_at"`
	MergedAt string `json:"merged_at"`
	Head     struct {
		SHA string `json:"sha"`
	} `json:"head"`
}
type apiCheck struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}
type status struct {
	ID        int64  `json:"id"`
	Context   string `json:"context"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
}

func configDir() (string, error) {
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	d := filepath.Join(h, ".config", "codex-pr-monitor")
	e = os.MkdirAll(d, 0700)
	return d, e
}
func dbOpen(d string) (*sql.DB, error) {
	db, e := sql.Open("sqlite", filepath.Join(d, "state.db"))
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS prs(url TEXT NOT NULL, thread TEXT NOT NULL, PRIMARY KEY(url,thread)); CREATE TABLE IF NOT EXISTS joins(url TEXT NOT NULL, thread TEXT NOT NULL, cutoff TEXT NOT NULL, PRIMARY KEY(url,thread)); CREATE TABLE IF NOT EXISTS terminal_deliveries(url TEXT NOT NULL, thread TEXT NOT NULL, event TEXT NOT NULL, PRIMARY KEY(url,thread,event)); CREATE TABLE IF NOT EXISTS seen(url TEXT NOT NULL, event TEXT NOT NULL, PRIMARY KEY(url,event)); CREATE TABLE IF NOT EXISTS inbox_items(id INTEGER PRIMARY KEY AUTOINCREMENT, url TEXT NOT NULL, thread TEXT NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL, UNIQUE(url,thread,event)); CREATE TABLE IF NOT EXISTS inbox_wakes(id INTEGER PRIMARY KEY AUTOINCREMENT, url TEXT NOT NULL, thread TEXT NOT NULL, attempts INTEGER NOT NULL, sent_at INTEGER NOT NULL, UNIQUE(url,thread));`)
	if e != nil {
		db.Close()
		return nil, e
	}
	// Lock before examining the schema so concurrent processes cannot migrate twice.
	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}
	var schema string
	err = conn.QueryRowContext(context.Background(), "SELECT sql FROM sqlite_master WHERE type='table' AND name='prs'").Scan(&schema)
	if err == nil && strings.Contains(strings.ToUpper(schema), "URL TEXT PRIMARY KEY") {
		_, err = conn.ExecContext(context.Background(), `CREATE TABLE prs_multi(url TEXT NOT NULL, thread TEXT NOT NULL, PRIMARY KEY(url,thread)); INSERT INTO prs_multi SELECT url,thread FROM prs; DROP TABLE prs; ALTER TABLE prs_multi RENAME TO prs; INSERT OR IGNORE INTO inbox_items(url,thread,event,detail) SELECT p.url,p.thread,i.event,i.detail FROM prs p JOIN inbox_items i ON i.url=p.url AND i.thread<>p.thread;`)
	}
	if err == nil {
		_, err = conn.ExecContext(context.Background(), "COMMIT")
	} else {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	}
	conn.Close()
	if err != nil {
		db.Close()
		return nil, err
	}

	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS subscription_generation(id INTEGER PRIMARY KEY AUTOINCREMENT, url TEXT NOT NULL, thread TEXT NOT NULL, UNIQUE(url,thread));
CREATE TRIGGER IF NOT EXISTS prs_new_generation AFTER INSERT ON prs BEGIN DELETE FROM subscription_generation WHERE url=NEW.url AND thread=NEW.thread; INSERT INTO subscription_generation(url,thread) VALUES(NEW.url,NEW.thread); END;
INSERT OR IGNORE INTO subscription_generation(url,thread) SELECT url,thread FROM prs;`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func validURL(s string) (string, []string, error) {
	m := prPattern.FindStringSubmatch(s)
	if m == nil {
		return "", nil, errors.New("expected https://github.com/OWNER/REPO/pull/NUMBER")
	}
	return strings.TrimSuffix(s, "/"), m, nil
}
func codexPath() (string, error) {
	if p, e := exec.LookPath("codex"); e == nil {
		return p, nil
	}
	p := "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"
	if s, e := os.Stat(p); e == nil && s.Mode()&0111 != 0 {
		return p, nil
	}
	return "", errors.New("codex not found on PATH or in ChatGPT.app; install Codex CLI")
}
func gh(endpoint string, dst any) error {
	if !githubAvailable() {
		return errors.New("GitHub rate limit backoff active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "--include", "--paginate", endpoint) // gh honors GH_TOKEN before GITHUB_TOKEN, so explicitly prefer the requested token.
	cmd.Env = ghEnv()
	out, e := cmd.Output()
	var stderr string
	if x, ok := e.(*exec.ExitError); ok {
		stderr = string(x.Stderr)
	}
	header, body, status, parseErr := splitGHPages(out)
	if parseErr != nil && e == nil {
		return parseErr
	}
	if isRateLimited(header, status, stderr) {
		noteRateLimit(header)
		return errors.New("GitHub rate limited the monitor; backing off")
	}
	if e != nil {
		return fmt.Errorf("gh api %s failed: %w", endpoint, e)
	}
	if e = json.Unmarshal(body, dst); e != nil {
		return fmt.Errorf("parse %s: %w", endpoint, e)
	}
	return nil
}
func active(db *sql.DB, r registration) bool {
	var t string
	e := db.QueryRow("SELECT thread FROM prs WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&t)
	return e == nil
}
func already(db *sql.DB, u, k string) bool {
	var n int
	_ = db.QueryRow("SELECT count(*) FROM seen WHERE url=? AND event=?", u, k).Scan(&n)
	return n > 0
}
func queue(path string, r registration, msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "queue", "--thread", r.Thread, "--message", msg, "--resume")
	out, e := cmd.CombinedOutput()
	if e != nil {
		return fmt.Errorf("codex queue failed: %w", e)
	}
	if resumeWarning.Match(out) {
		log.Printf("%s: message queued, but Codex could not resume thread %s; check the chat if it does not resume", r.URL, r.Thread)
	}
	return nil
}
func scan(db *sql.DB, r registration, path string) error { return scanSnapshot(db, r, path, nil) }
func scanSnapshot(db *sql.DB, r registration, path string, snapshot *gqlSnapshot) error {
	_, m, e := validURL(r.URL)
	if e != nil {
		return e
	}
	base := fmt.Sprintf("repos/%s/%s", m[1], m[2])
	pr := base + "/pulls/" + m[3]
	var p pull
	if snapshot != nil {
		p.State = strings.ToLower(snapshot.State)
		if p.State == "merged" {
			p.State = "closed"
		}
		p.ClosedAt = snapshot.ClosedAt
		p.MergedAt = snapshot.MergedAt
		p.Head.SHA = snapshot.Head
	} else {
		var pulls []pull
		if e = gh(pr, &pulls); e != nil {
			return e
		}
		if len(pulls) != 1 {
			return errors.New("unexpected pull response")
		}
		p = pulls[0]
	}
	if p.State == "closed" {
		state, at := "closed", p.ClosedAt
		if p.MergedAt != "" {
			state, at = "merged", p.MergedAt
		}
		if at == "" {
			return errors.New("closed PR is missing its closure timestamp")
		}
		key := "terminal:" + state + ":" + at
		detail := fmt.Sprintf("PR %s at %s. Monitoring removed; register again if reopened.", state, at)
		if e := saveEvents(db, r, []string{key}, []string{detail}); e != nil {
			return e
		}
		return wakeOwners(db, path, r.URL, key)

	}
	if p.State != "open" {
		return fmt.Errorf("unexpected PR state %q", p.State)
	}
	head := url.PathEscape(p.Head.SHA)
	var reviews [][]apiReview
	var issueComments, reviewComments [][]apiComment
	var checks []struct {
		CheckRuns []apiCheck `json:"check_runs"`
	}
	var statuses [][]status
	var haveReviews, haveIssue, haveInline bool
	if snapshot != nil {
		reviews, haveReviews = snapshot.restReviews()
		issueComments, haveIssue = snapshot.restComments(false)
		reviewComments, haveInline = snapshot.restComments(true)
	}
	if !haveReviews {
		if e = gh(pr+"/reviews?per_page=100", &reviews); e != nil {
			return e
		}
	}
	if !haveIssue {
		if e = gh(base+"/issues/"+m[3]+"/comments?per_page=100", &issueComments); e != nil {
			return e
		}
	}
	if !haveInline {
		if e = gh(pr+"/comments?per_page=100", &reviewComments); e != nil {
			return e
		}
	}
	if e = gh(base+"/commits/"+head+"/check-runs?per_page=100", &checks); e != nil {
		return e
	}
	if e = gh(base+"/commits/"+head+"/statuses?per_page=100", &statuses); e != nil {
		return e
	}
	var keys, lines []string
	statusTimes := map[string]string{}
	add := func(k, l string) {
		keys = append(keys, k)
		lines = append(lines, l)
	}
	for _, page := range reviews {
		for _, v := range page {
			if v.SubmittedAt != nil && v.State != "PENDING" {
				add(fmt.Sprintf("review:%d:%s", v.ID, *v.SubmittedAt), fmt.Sprintf("Submitted review %d by %s.", v.ID, v.User.Login))
			}
		}
	}
	for _, pages := range [][][]apiComment{issueComments, reviewComments} {
		for _, page := range pages {
			for _, v := range page {
				add(fmt.Sprintf("comment:%d:%s", v.ID, v.UpdatedAt), fmt.Sprintf("Comment %d at %s by %s.", v.ID, v.HTMLURL, v.User.Login))
			}
		}
	}
	for _, page := range checks {
		for _, v := range page.CheckRuns {
			if v.Status == "completed" && (v.Conclusion == "failure" || v.Conclusion == "timed_out" || v.Conclusion == "action_required") {
				add(fmt.Sprintf("check:%s:%d:%s:%s", p.Head.SHA, v.ID, v.Conclusion, v.CompletedAt), fmt.Sprintf("Failed current-head check %q (run %d, head %s).", v.Name, v.ID, p.Head.SHA))
			}
		}
	}
	// Only the latest status for each context describes the current result.
	contexts := map[string]bool{}
	for _, page := range statuses {
		for _, v := range page {
			if contexts[v.Context] {
				continue
			}
			contexts[v.Context] = true
			if v.State == "failure" || v.State == "error" {
				key := fmt.Sprintf("status:%s:%d:%s", p.Head.SHA, v.ID, v.State)
				statusTimes[key] = v.UpdatedAt
				add(key, fmt.Sprintf("Failed current-head status %q (head %s).", v.Context, p.Head.SHA))
			}
		}
	}
	if e = saveEventsWithTimes(db, r, keys, lines, statusTimes); e != nil {
		return e
	}
	return wakeOwners(db, path, r.URL, "")
}

func pollBatch(db *sql.DB, path string, batch []registration) bool {
	if len(batch) == 0 || !githubAvailable() {
		return false
	}
	snapshots, err := ghGraphQL(batch)
	if err != nil {
		log.Printf("GitHub batch: %v", err)
		if !githubAvailable() {
			return false
		}
		if errors.Is(err, errGraphQLRejected) && len(batch) > 1 {
			middle := len(batch) / 2
			a := pollBatch(db, path, batch[:middle])
			b := pollBatch(db, path, batch[middle:])
			return a && b
		}
		if errors.Is(err, errGraphQLUnsupported) || errors.Is(err, errGraphQLRejected) {
			ok := true
			for _, r := range batch {
				if active(db, r) {
					if e := scan(db, r, path); e != nil {
						ok = false
						log.Printf("%s: %v", r.URL, e)
					}
				}
				if !githubAvailable() {
					return false
				}
			}
			return ok
		}
		return false
	}
	ok := true
	for i, r := range batch {
		if !active(db, r) {
			continue
		}
		if snapshots[i].err != nil {
			ok = false
			log.Printf("%s: %v", r.URL, snapshots[i].err)
			continue
		}
		if e := scanSnapshot(db, r, path, snapshots[i].snap); e != nil {
			ok = false
			log.Printf("%s: %v", r.URL, e)
		}
		if !githubAvailable() {
			return false
		}
	}
	return ok
}
func poll(db *sql.DB, path string) {
	flushInbox(db, path)
	if !githubAvailable() {
		return
	}
	rows, e := db.Query("SELECT url, min(thread) FROM prs GROUP BY url ORDER BY url")
	if e != nil {
		log.Print(e)
		return
	}
	var rs []registration
	for rows.Next() {
		var r registration
		if e := rows.Scan(&r.URL, &r.Thread); e != nil {
			log.Print(e)
			rows.Close()
			return
		}
		rs = append(rs, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		log.Print(e)
		return
	}
	healthy := true
	for start := 0; start < len(rs); start += batchSize {
		end := start + batchSize
		if end > len(rs) {
			end = len(rs)
		}
		if !pollBatch(db, path, rs[start:end]) {
			healthy = false
		}
		if !githubAvailable() {
			return
		}
	}
	if healthy {
		rateFailures = 0
	}
}
func daemon(d, path string) error {
	f := os.NewFile(3, "lock")
	if f == nil {
		return errors.New("missing lock")
	}
	defer f.Close()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sig)
	pidPath := filepath.Join(d, "pid")
	tmpPath := filepath.Join(d, "pid.tmp")
	if e := os.WriteFile(tmpPath, []byte(fmt.Sprint(os.Getpid())), 0600); e != nil {
		return e
	}
	defer os.Remove(tmpPath)
	if e := os.Rename(tmpPath, pidPath); e != nil {
		return e
	}
	defer os.Remove(pidPath)
	db, e := dbOpen(d)
	if e != nil {
		return e
	}
	defer db.Close()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	poll(db, path)
	for {
		select {
		case <-sig:
			return nil
		case <-ticker.C:
			poll(db, path)
		}
	}
}
func start(d string) error {
	lock, e := os.OpenFile(filepath.Join(d, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		if errors.Is(e, syscall.EWOULDBLOCK) || errors.Is(e, syscall.EAGAIN) {
			return nil
		}
		return e
	}
	p, e := codexPath()
	if e != nil {
		return e
	}
	if _, e = exec.LookPath("gh"); e != nil {
		return errors.New("gh not found on PATH; install and authenticate GitHub CLI")
	}
	if e = os.Remove(filepath.Join(d, "pid")); e != nil && !os.IsNotExist(e) {
		return e
	}
	logfile, e := os.OpenFile(filepath.Join(d, "monitor.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	defer logfile.Close()
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(exe, "_daemon", p)
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return e
	}
	return cmd.Process.Release()
}
func stop(d string) error {
	lock, e := os.OpenFile(filepath.Join(d, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e == nil {
		return nil
	}
	if !errors.Is(e, syscall.EWOULDBLOCK) && !errors.Is(e, syscall.EAGAIN) {
		return e
	}
	b, e := os.ReadFile(filepath.Join(d, "pid"))
	if e != nil {
		return errors.New("monitor is starting; retry stop shortly")
	}
	var pid int
	if _, e = fmt.Sscan(string(b), &pid); e != nil || pid <= 1 {
		return errors.New("invalid monitor PID")
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: codex-pr-monitor start|stop|add|remove|inbox|ack")
	}
	d, e := configDir()
	if e != nil {
		return e
	}
	switch args[0] {
	case "start":
		return start(d)
	case "stop":
		return stop(d)
	case "_daemon":
		if len(args) != 2 {
			return errors.New("missing codex path")
		}
		return daemon(d, args[1])
	case "add", "remove", "inbox", "ack":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		u := fs.String("url", "", "PR URL")
		t := fs.String("thread", "", "owning thread ID")
		all := fs.Bool("all-threads", false, "remove all owners")
		wake := fs.Int64("wake", -1, "wake ID from inbox")
		through := fs.Int64("through", 0, "last handled inbox item")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		normalized, _, e := validURL(*u)
		if e != nil {
			return e
		}
		db, e := dbOpen(d)
		if e != nil {
			return e
		}
		defer db.Close()
		if *all && args[0] != "remove" {
			return errors.New("--all-threads is only valid for remove")
		}
		if args[0] == "add" {
			if strings.TrimSpace(*t) == "" {
				return errors.New("--thread is required")
			}
			e = addOwner(db, registration{normalized, *t})
		} else if args[0] == "remove" {
			e = removeOwners(db, normalized, *t, *all)
		} else if args[0] == "inbox" {
			e = showInbox(db, normalized, *t, os.Stdout)
		} else {
			e = ackInbox(db, normalized, *t, *wake, *through)
		}
		return e
	default:
		return errors.New("usage: codex-pr-monitor start|stop|add|remove|inbox|ack")
	}
}
func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
