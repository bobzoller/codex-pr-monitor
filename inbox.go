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
	if len(keys) != len(details) {
		return errors.New("invalid event batch")
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var owner string
	if e = tx.QueryRow("SELECT thread FROM prs WHERE url=?", r.URL).Scan(&owner); e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	if owner != r.Thread {
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
		var pendingOther int
		if n == 0 {
			if e = tx.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread<>? AND event=?", r.URL, r.Thread, k).Scan(&pendingOther); e != nil {
				return e
			}
		}
		if n != 0 || pendingOther > 0 {
			if _, e = tx.Exec("INSERT OR IGNORE INTO inbox_items(url,thread,event,detail) VALUES(?,?,?,?)", r.URL, r.Thread, k, details[i]); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
func pendingForAnotherOwner(db *sql.DB, r registration, key string) bool {
	var n int
	_ = db.QueryRow("SELECT count(*) FROM inbox_items WHERE url=? AND thread<>? AND event=?", r.URL, r.Thread, key).Scan(&n)
	return n > 0
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
	var owner string
	err := tx.QueryRow("SELECT thread FROM prs WHERE url=?", r.URL).Scan(&owner)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && owner != r.Thread {
		return false, nil
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
	if e = tx.Commit(); e != nil {
		return false, e
	}
	if attempts > 0 && time.Since(time.Unix(sent, 0)) < wakeRetryAfter {
		return true, nil
	}
	if attempts >= 2 {
		if attempts == 2 {
			fmt.Fprintf(os.Stderr, "%s: inbox still unacknowledged after retry; inspect owning chat\n", r.URL)
			_, _ = db.Exec("UPDATE inbox_wakes SET attempts=3 WHERE id=? AND attempts=2", id)
		}
		return true, nil
	}
	if e = queue(path, r, wakeMessage(r, terminal.String)); e != nil {
		if attempts == 0 {
			_, _ = db.Exec("DELETE FROM inbox_wakes WHERE id=? AND attempts=0", id)
		} else {
			_, _ = db.Exec("UPDATE inbox_wakes SET sent_at=? WHERE id=?", time.Now().Unix(), id)
		}
		return attempts > 0, e
	}
	_, e = db.Exec("UPDATE inbox_wakes SET attempts=attempts+1,sent_at=? WHERE id=?", time.Now().Unix(), id)
	return true, e
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
