package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const wakeRetryAfter = 2 * time.Hour

// saveEvents commits the inbox and deduplication checkpoint together.
func saveEvents(db *sql.DB, r registration, keys, details []string) error {
	return saveEventsWithTimes(db, r, keys, details, nil)
}
func saveEventsWithTimes(db *sql.DB, r registration, keys, details []string, times map[string]string) error {
	if len(keys) != len(details) {
		return errors.New("invalid event batch")
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var owners int
	if e = tx.QueryRow("SELECT count(*) FROM prs WHERE url=?", r.URL).Scan(&owners); e != nil {
		return e
	}
	if owners == 0 {
		return nil
	}
	for i, k := range keys {
		res, e := tx.Exec("INSERT OR IGNORE INTO seen(url,event) VALUES(?,?)", r.URL, k)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		terminal := strings.HasPrefix(k, "terminal:")
		if n == 0 && !terminal {
			continue
		}
		at := eventTime(k)
		if raw, ok := times[k]; ok {
			at = eventTime("comment:0:" + raw)
		}
		rows, e := tx.Query(`SELECT prs.thread, COALESCE(joins.cutoff,'') FROM prs LEFT JOIN joins ON joins.url=prs.url AND joins.thread=prs.thread WHERE prs.url=?`, r.URL)
		if e != nil {
			return e
		}
		var targets []string
		for rows.Next() {
			var thread, cutoff string
			if e = rows.Scan(&thread, &cutoff); e != nil {
				break
			}
			if (terminal && (n != 0 || cutoff != "")) || (!terminal && (cutoff == "" || (at != "" && at > cutoff))) {
				targets = append(targets, thread)
			}
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		for _, thread := range targets {
			if terminal {
				res, err := tx.Exec("INSERT OR IGNORE INTO terminal_deliveries VALUES(?,?,?)", r.URL, thread, k)
				if err != nil {
					return err
				}
				count, err := res.RowsAffected()
				if err != nil {
					return err
				}
				if count == 0 {
					continue
				}
			}
			if _, e = tx.Exec("INSERT OR IGNORE INTO inbox_items(url,thread,event,detail) VALUES(?,?,?,?)", r.URL, thread, k, details[i]); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}

// Normalize event-key timestamps to fixed-width UTC for comparison. Status timestamps are supplied separately.
func eventTime(key string) string {
	parts := strings.Split(key, ":")
	i := 0
	switch parts[0] {
	case "review", "comment", "terminal":
		i = 2
	case "status":
		i = 3
	case "check":
		i = 4
	default:
		return ""
	}
	if len(parts) <= i {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, strings.Join(parts[i:], ":"))
	if err != nil {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func addOwner(db *sql.DB, r registration) error {
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// The insert takes the SQLite write lock before we inspect subscribers.
	res, e := tx.Exec("INSERT OR IGNORE INTO prs(url,thread) VALUES(?,?)", r.URL, r.Thread)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n != 0 {
		var count int
		if e = tx.QueryRow("SELECT count(*) FROM prs WHERE url=?", r.URL).Scan(&count); e != nil {
			return e
		}
		if count > 1 {
			_, e = tx.Exec("INSERT OR REPLACE INTO joins VALUES(?,?,?)", r.URL, r.Thread, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
			if e != nil {
				return e
			}
		} else {
			if _, e = tx.Exec("DELETE FROM joins WHERE url=? AND thread=?", r.URL, r.Thread); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
func removeOwners(db *sql.DB, url, thread string, all bool) error {
	if all && thread != "" {
		return errors.New("--thread and --all-threads are mutually exclusive")
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// Serialize removal with concurrent registration and delivery.
	if _, e = tx.Exec("UPDATE prs SET thread=thread WHERE url=?", url); e != nil {
		return e
	}
	if !all && strings.TrimSpace(thread) == "" {
		return errors.New("specify --thread or --all-threads")
	}

	for _, table := range []string{"prs", "joins", "inbox_items", "inbox_wakes"} {
		q := "DELETE FROM " + table + " WHERE url=?"
		args := []any{url}
		if !all {
			q += " AND thread=?"
			args = append(args, thread)
		}
		if _, e = tx.Exec(q, args...); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func wakeOwners(db *sql.DB, path, url, terminal string) error {
	rows, e := db.Query("SELECT g.id,p.thread FROM prs p JOIN subscription_generation g ON g.url=p.url AND g.thread=p.thread WHERE p.url=? ORDER BY p.thread", url)
	if e != nil {
		return e
	}
	type owner struct {
		id     int64
		thread string
	}
	var owners []owner
	for rows.Next() {
		var o owner
		if e = rows.Scan(&o.id, &o.thread); e != nil {
			break
		}
		owners = append(owners, o)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	var first error
	for _, o := range owners {
		r := registration{url, o.thread}
		sent, err := sendWake(db, path, r)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if terminal != "" {
			var pending int
			err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread=? AND event LIKE 'terminal:%'", url, o.thread).Scan(&pending)
			if err == nil && (pending > 0 && sent || pending == 0) {
				// A late join may not yet have been included in the terminal snapshot.
				_, err = db.Exec(`DELETE FROM prs WHERE url=? AND thread=? AND EXISTS (SELECT 1 FROM subscription_generation WHERE id=? AND url=? AND thread=?) AND (? > 0 OR NOT EXISTS (SELECT 1 FROM joins WHERE url=? AND thread=?) OR EXISTS (SELECT 1 FROM terminal_deliveries WHERE url=? AND thread=? AND event=?))`, url, o.thread, o.id, url, o.thread, pending, url, o.thread, url, o.thread, terminal)
			}
			if err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func shellArg(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func wakeMessage(r registration, terminal string) string {
	prefix := "PR monitor: update"
	if terminal != "" {
		prefix = "PR monitor: " + terminal + "; monitoring removed"
	}
	return fmt.Sprintf("%s. Run `codex-pr-monitor inbox --url %s --thread %s`", prefix, shellArg(r.URL), shellArg(r.Thread))
}

// A persisted wake is shared by all pending events for this PR and owner.
// A single delayed retry handles a lost resume without generating a stream of duplicates.
func sendWake(db *sql.DB, path string, r registration) (bool, error) {
	tx, e := db.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	// Serialize queueing against removal and acknowledgement in other processes.
	if _, e = tx.Exec("UPDATE inbox_items SET detail=detail WHERE url=? AND thread=?", r.URL, r.Thread); e != nil {
		return false, e
	}
	var count int
	var terminal sql.NullString
	e = tx.QueryRow(`SELECT count(*),max(CASE WHEN event LIKE 'terminal:merged:%' THEN 'merged' WHEN event LIKE 'terminal:closed:%' THEN 'closed' END) FROM inbox_items WHERE url=? AND thread=?`, r.URL, r.Thread).Scan(&count, &terminal)
	if e != nil {
		return false, e
	}
	if count == 0 {
		return false, nil
	}
	if _, e = tx.Exec(`INSERT OR IGNORE INTO inbox_wakes(url,thread,attempts,sent_at) VALUES(?,?,0,0)`, r.URL, r.Thread); e != nil {
		return false, e
	}
	var id int64
	var attempts int
	var sent int64
	if e = tx.QueryRow("SELECT id,attempts,sent_at FROM inbox_wakes WHERE url=? AND thread=?", r.URL, r.Thread).Scan(&id, &attempts, &sent); e != nil {
		return false, e
	}
	if attempts > 0 && time.Since(time.Unix(sent, 0)) < wakeRetryAfter {
		return true, tx.Commit()
	}
	if attempts >= 2 {
		if attempts == 2 {
			fmt.Fprintf(os.Stderr, "%s: inbox still unacknowledged after retry; inspect owning chat\n", r.URL)
			_, _ = tx.Exec("UPDATE inbox_wakes SET attempts=3 WHERE id=? AND attempts=2", id)
		}
		return true, tx.Commit()
	}
	if e = queue(path, r, wakeMessage(r, terminal.String)); e != nil {
		if attempts == 0 {
			_, _ = tx.Exec("DELETE FROM inbox_wakes WHERE id=? AND attempts=0", id)
		} else {
			_, _ = tx.Exec("UPDATE inbox_wakes SET sent_at=? WHERE id=?", time.Now().Unix(), id)
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return attempts > 0, e
	}
	_, e = tx.Exec("UPDATE inbox_wakes SET attempts=attempts+1,sent_at=? WHERE id=?", time.Now().Unix(), id)
	if e != nil {
		return false, e
	}
	return true, tx.Commit()
}
func flushInbox(db *sql.DB, path string) {
	rows, e := db.Query("SELECT DISTINCT url,thread FROM inbox_items ORDER BY url,thread")
	if e != nil {
		fmt.Fprintln(os.Stderr, "monitor inbox:", e)
		return
	}
	var all []registration
	for rows.Next() {
		var r registration
		if e = rows.Scan(&r.URL, &r.Thread); e != nil {
			break
		}
		all = append(all, r)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		fmt.Fprintln(os.Stderr, "monitor inbox:", e)
		return
	}
	for _, r := range all {
		if _, e = sendWake(db, path, r); e != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", r.URL, e)
		}
	}
}
func resolveInboxThread(db *sql.DB, url, thread string) (string, error) {
	if thread == "" {
		thread = os.Getenv("CODEX_THREAD_ID")
	}
	if thread != "" {
		return thread, nil
	}
	rows, e := db.Query("SELECT DISTINCT thread FROM inbox_items WHERE url=?", url)
	if e != nil {
		return "", e
	}
	defer rows.Close()
	var owner string
	for rows.Next() {
		var t string
		if e = rows.Scan(&t); e != nil {
			return "", e
		}
		if owner != "" && owner != t {
			return "", errors.New("multiple owning threads have pending items; specify --thread")
		}
		owner = t
	}
	return owner, rows.Err()
}
func showInbox(db *sql.DB, url, thread string, out io.Writer) error {
	thread, e := resolveInboxThread(db, url, thread)
	if e != nil {
		return e
	}
	if thread == "" {
		fmt.Fprintln(out, "Inbox empty.")
		return nil
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var wake int64
	e = tx.QueryRow("SELECT id FROM inbox_wakes WHERE url=? AND thread=?", url, thread).Scan(&wake)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	rows, e := tx.Query("SELECT id,detail FROM inbox_items WHERE url=? AND thread=? ORDER BY id", url, thread)
	if e != nil {
		return e
	}
	var last int64
	var items []string
	for rows.Next() {
		var id int64
		var detail string
		if e = rows.Scan(&id, &detail); e != nil {
			break
		}
		last = id
		items = append(items, fmt.Sprintf("- %s", detail))
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "Inbox empty.")
		return nil
	}
	fmt.Fprintln(out, strings.Join(items, "\n"))
	fmt.Fprintf(out, "After handling these items, acknowledge only this snapshot:\ncodex-pr-monitor ack --url %s --thread %s --wake %d --through %d\n", shellArg(url), shellArg(thread), wake, last)
	return nil
}
func ackInbox(db *sql.DB, url, thread string, wake, through int64) error {
	if wake < 0 || through <= 0 {
		return errors.New("--wake and --through from the inbox are required")
	}
	thread, e := resolveInboxThread(db, url, thread)
	if e != nil {
		return e
	}
	if thread == "" {
		return errors.New("no pending inbox for PR")
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var current int64
	e = tx.QueryRow("SELECT id FROM inbox_wakes WHERE url=? AND thread=?", url, thread).Scan(&current)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if current != wake {
		return errors.New("wake changed; read the inbox again")
	}
	var maximum sql.NullInt64
	if e = tx.QueryRow("SELECT max(id) FROM inbox_items WHERE url=? AND thread=?", url, thread).Scan(&maximum); e != nil {
		return e
	}
	if !maximum.Valid || through > maximum.Int64 {
		return errors.New("invalid inbox cursor; read the inbox again")
	}
	if _, e = tx.Exec("DELETE FROM inbox_items WHERE url=? AND thread=? AND id<=?", url, thread, through); e != nil {
		return e
	}
	if current != 0 {
		if _, e = tx.Exec("DELETE FROM inbox_wakes WHERE id=?", current); e != nil {
			return e
		}
	}
	return tx.Commit()
}
