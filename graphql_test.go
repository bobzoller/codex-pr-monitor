package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGraph(t *testing.T, d, s string) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(d, "graphql.json"), []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}

const openGraph = `{"state":"OPEN","headRefOid":"abc","reviews":{"nodes":[{"fullDatabaseId":"1","state":"COMMENTED","submittedAt":"2026-01-01","body":"","author":{"login":"bobzoller","__typename":"User"}},{"fullDatabaseId":"2","state":"PENDING","submittedAt":null,"body":"draft","author":{"login":"bobzoller","__typename":"User"}}],"pageInfo":{"hasNextPage":false}},"comments":{"nodes":[{"fullDatabaseId":"4","body":"finding","updatedAt":"2026-01-01","url":"https://example/4","author":{"login":"greptile-apps","__typename":"Bot"}}],"pageInfo":{"hasNextPage":false}},"reviewThreads":{"nodes":[{"comments":{"nodes":[{"fullDatabaseId":"10","state":"PENDING","body":"draft","updatedAt":"2026-01-01","url":"https://example/draft","author":{"login":"greptile-apps","__typename":"Bot"}}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}`
const closedGraph = `{"state":"MERGED","closedAt":"2026-01-02T00:00:00Z","mergedAt":"2026-01-02T00:00:00Z","headRefOid":"abc","reviews":{"nodes":[],"pageInfo":{"hasNextPage":false}},"comments":{"nodes":[],"pageInfo":{"hasNextPage":false}},"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}`

func TestGraphQLBatchAndDurableLegacyIDs(t *testing.T) {
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	r2 := registration{"https://github.com/acme/repo/pull/2", "thread-2"}
	for _, p := range []registration{*r, r2} {
		if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", p.URL, p.Thread); e != nil {
			t.Fatal(e)
		}
	}
	// IDs written by the REST implementation must suppress the same GraphQL events.
	for _, k := range []string{"review:1:2026-01-01", "comment:4:2026-01-01", "check:abc:6:failure:2026-01-01"} {
		if _, e = db.Exec("INSERT INTO seen VALUES(?,?)", r.URL, k); e != nil {
			t.Fatal(e)
		}
	}
	writeGraph(t, d, fmt.Sprintf(`{"data":{"viewer":{"login":"bobzoller"},"p0":{"pullRequest":%s},"p1":{"pullRequest":%s}}}`, openGraph, closedGraph))
	poll(db, c)
	if !active(db, *r) || active(db, r2) {
		t.Fatal("open and merged registrations not handled correctly")
	}
	b, e := os.ReadFile(filepath.Join(d, "queued"))
	if e != nil {
		t.Fatal(e)
	}
	out := string(b)
	if strings.Count(out, "PR monitor:") != 1 || !strings.Contains(out, "merged; monitoring removed") || strings.Contains(out, "thread-1") || strings.Contains(out, "draft") {
		t.Fatalf("unexpected deliveries %s", out)
	}
	calls, _ := os.ReadFile(filepath.Join(d, "requests"))
	if strings.Count(string(calls), "graphql") != 1 || strings.Contains(string(calls), "/reviews?") || strings.Contains(string(calls), "/comments?") {
		t.Fatalf("unexpected API calls: %s", calls)
	}
}
func TestGraphQLPartialErrorDoesNotBlockOtherPRs(t *testing.T) {
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	r2 := registration{"https://github.com/acme/repo/pull/2", "thread-2"}
	for _, p := range []registration{*r, r2} {
		if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", p.URL, p.Thread); e != nil {
			t.Fatal(e)
		}
	}
	writeGraph(t, d, fmt.Sprintf(`{"data":{"viewer":{"login":"bobzoller"},"p0":{"pullRequest":%s},"p1":null},"errors":[{"message":"forbidden","path":["p1","pullRequest"]}]}`, closedGraph))
	if e = os.WriteFile(filepath.Join(d, "gqlfail"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	poll(db, c)
	if active(db, *r) || !active(db, r2) {
		t.Fatal("partial error should preserve only failed PR")
	}
	b, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(b), "PR monitor:") != 1 {
		t.Fatalf("wrong messages: %s", b)
	}
}
func TestGraphQLPaginationFallsBackToREST(t *testing.T) {
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	var s gqlSnapshot
	if e = json.Unmarshal([]byte(openGraph), &s); e != nil {
		t.Fatal(e)
	}
	s.Reviews.Page.HasNextPage = true
	s.Comments.Page.HasNextPage = true
	s.Threads.Nodes[0].Comments.Page.HasNextPage = true
	if e = scanSnapshot(db, *r, c, &s); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "requests"))
	out := string(b)
	for _, want := range []string{"/reviews?", "/issues/1/comments?", "/pulls/1/comments?"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing fallback %s: %s", want, out)
		}
	}
	var q bytes.Buffer
	if e = showInbox(db, r.URL, r.Thread, &q); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(q.String(), "review 1") || !strings.Contains(q.String(), "Comment 4") {
		t.Fatalf("missing feedback %s", q.String())
	}
}
func TestSplitGHPages(t *testing.T) {
	raw := []byte("HTTP/2.0 200 OK\nX-RateLimit-Remaining: 50\r\n\r\n[1,2]\nHTTP/2.0 200 OK\nX-RateLimit-Remaining: 49\r\n\r\n[3]\n")
	h, b, status, e := splitGHPages(raw)
	if e != nil || status != 200 || string(b) != "[[1,2],[3]]" || h.Get("X-Ratelimit-Remaining") != "49" {
		t.Fatalf("pages: %s %v %d %v", b, h, status, e)
	}
}
func TestGraphQLRateLimitBackoff(t *testing.T) {
	r, d, _ := setup(t)
	previous := rateLimitedUntil
	defer func() { rateLimitedUntil = previous }()
	rateLimitedUntil = time.Time{}
	writeGraph(t, d, "HTTP/2.0 429 Too Many Requests\nRetry-After: 123\r\n\r\n{\"message\":\"limited\"}")
	if _, e := ghGraphQL([]registration{*r}); e == nil {
		t.Fatal("expected rate limit")
	}
	if time.Until(rateLimitedUntil) < 120*time.Second {
		t.Fatalf("retry-after ignored: %v", rateLimitedUntil)
	}
	if _, e := ghGraphQL([]registration{*r}); e == nil {
		t.Fatal("expected active backoff")
	}
	b, _ := os.ReadFile(filepath.Join(d, "requests"))
	if strings.Count(string(b), "graphql") != 1 {
		t.Fatalf("backoff sent another request: %s", b)
	}
}
func TestGraphQLBatchEscapesNames(t *testing.T) {
	query, e := gqlQuery([]registration{{URL: `https://github.com/acme/repo/pull/1`}})
	if e != nil || !strings.Contains(query, `repository(owner:"acme",name:"repo")`) {
		t.Fatalf("unexpected query: %s %v", query, e)
	}
}

func TestGraphQLSchemaFallbackAndUnchangedQueue(t *testing.T) {
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	writeGraph(t, d, `{"errors":[{"message":"Field 'fullDatabaseId' doesn't exist on type 'PullRequestReview'"}]}`)
	if e = os.WriteFile(filepath.Join(d, "gqlfail"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	poll(db, c)
	var b bytes.Buffer
	if e = showInbox(db, r.URL, r.Thread, &b); e != nil || !strings.Contains(b.String(), "review 1") {
		t.Fatalf("REST schema fallback failed: %s %v", b.String(), e)
	}
}
func TestGraphQLCommentTextCannotTriggerRateLimit(t *testing.T) {
	r, d, _ := setup(t)
	prev, failures := rateLimitedUntil, rateFailures
	defer func() { rateLimitedUntil = prev; rateFailures = failures }()
	rateLimitedUntil = time.Time{}
	rateFailures = 0
	graph := strings.Replace(openGraph, "finding", "rate limit", 1)
	writeGraph(t, d, fmt.Sprintf(`{"data":{"viewer":{"login":"bobzoller"},"p0":{"pullRequest":%s}}}`, graph))
	if _, e := ghGraphQL([]registration{*r}); e != nil {
		t.Fatalf("comment content triggered rate limiter: %v", e)
	}
}
func TestGraphQLIncompleteConnectionFallsBack(t *testing.T) {
	var s gqlSnapshot
	if e := json.Unmarshal([]byte(`{"reviews":{"nodes":[]},"comments":null}`), &s); e != nil {
		t.Fatal(e)
	}
	if _, ok := s.restReviews(); ok {
		t.Fatal("missing review pageInfo must fall back")
	}
	if _, ok := s.restComments(false); ok {
		t.Fatal("null comments must fall back")
	}
	if _, ok := s.restComments(true); ok {
		t.Fatal("missing threads must fall back")
	}
}
func TestRESTRateLimitBackoff(t *testing.T) {
	_, d, _ := setup(t)
	prev, failures := rateLimitedUntil, rateFailures
	defer func() { rateLimitedUntil = prev; rateFailures = failures }()
	rateLimitedUntil = time.Time{}
	rateFailures = 0
	script(t, filepath.Join(d, "bin"), "gh", `echo request >> "$HOME/rate_calls"; printf 'HTTP/2.0 429 Too Many Requests\nRetry-After: 123\r\n\r\n{"message":"limited"}'; exit 1`)
	var v [][]apiReview
	if e := gh("repos/acme/repo/pulls/1/reviews", &v); e == nil {
		t.Fatal("expected rate limit")
	}
	if time.Until(rateLimitedUntil) < 120*time.Second {
		t.Fatalf("retry-after ignored: %v", rateLimitedUntil)
	}
	if e := gh("repos/acme/repo/pulls/1/reviews", &v); e == nil {
		t.Fatal("backoff should prevent request")
	}
	b, _ := os.ReadFile(filepath.Join(d, "rate_calls"))
	if strings.Count(string(b), "request") != 1 {
		t.Fatal("made request during backoff")
	}
}
func TestRejectedGraphQLBatchSplitsAndContinues(t *testing.T) {
	r, d, c := setup(t)
	db, e := dbOpen(d)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	r2 := registration{"https://github.com/acme/repo/pull/2", "thread-2"}
	for _, p := range []registration{*r, r2} {
		if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", p.URL, p.Thread); e != nil {
			t.Fatal(e)
		}
	}
	writeGraph(t, d, fmt.Sprintf(`{"data":{"viewer":{"login":"bobzoller"},"p0":{"pullRequest":%s}}}`, closedGraph))
	script(t, filepath.Join(d, "bin"), "gh", `echo call >> "$HOME/split_calls"; case "$5" in *"p1: repository"*) echo '{"errors":[{"message":"resource limit exceeded"}]}'; exit 1;; *) cat "$HOME/graphql.json";; esac`)
	poll(db, c)
	if active(db, *r) || active(db, r2) {
		t.Fatal("split batches should both be processed")
	}
	b, _ := os.ReadFile(filepath.Join(d, "split_calls"))
	if strings.Count(string(b), "call") != 3 {
		t.Fatalf("expected 1 batch and 2 smaller queries: %s", b)
	}
	q, _ := os.ReadFile(filepath.Join(d, "queued"))
	if strings.Count(string(q), "merged; monitoring removed") != 2 {
		t.Fatalf("missing terminal notification: %s", q)
	}
}

func TestAllAuthorsAndDeduplication(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "GraphQL"
		if fallback {
			name = "REST"
		}
		t.Run(name, func(t *testing.T) {
			r, d, c := setup(t)
			db, err := dbOpen(d)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); err != nil {
				t.Fatal(err)
			}
			graph := `{"state":"OPEN","headRefOid":"abc","reviews":{"nodes":[{"fullDatabaseId":"11","state":"APPROVED","submittedAt":"date","author":{"login":"self","__typename":"User"}},{"fullDatabaseId":"12","state":"COMMENTED","submittedAt":"date","author":{"login":"other","__typename":"User"}},{"fullDatabaseId":"13","state":"COMMENTED","submittedAt":"date","author":{"login":"newbot","__typename":"Bot"}},{"fullDatabaseId":"14","state":"COMMENTED","submittedAt":"date","author":null},{"fullDatabaseId":"15","state":"PENDING","submittedAt":null}],"pageInfo":{"hasNextPage":false}},"comments":{"nodes":[{"fullDatabaseId":"21","updatedAt":"date","author":{"login":"self","__typename":"User"}},{"fullDatabaseId":"22","updatedAt":"date","author":{"login":"newbot","__typename":"Bot"}}],"pageInfo":{"hasNextPage":false}},"reviewThreads":{"nodes":[{"comments":{"nodes":[{"fullDatabaseId":"23","state":"SUBMITTED","updatedAt":"date","author":null},{"fullDatabaseId":"24","state":"PENDING","updatedAt":"date"}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}`
			if fallback {
				writeGraph(t, d, `{"errors":[{"message":"Cannot query field fullDatabaseId"}]}`)
			} else {
				writeGraph(t, d, fmt.Sprintf(`{"data":{"p0":{"pullRequest":%s}}}`, graph))
			}
			script(t, filepath.Join(d, "bin"), "gh", `if [ "$3" = graphql ]; then cat "$HOME/graphql.json"; exit 0; fi
case "$4" in
 */pulls/1) echo '[{"state":"open","head":{"sha":"abc"}}]' ;;
 */reviews*) echo '[[{"id":11,"state":"APPROVED","submitted_at":"date","user":{"login":"self","type":"User"}},{"id":12,"state":"COMMENTED","submitted_at":"date","user":{"login":"other","type":"User"}},{"id":13,"state":"COMMENTED","submitted_at":"date","user":{"login":"newbot","type":"Bot"}},{"id":14,"state":"COMMENTED","submitted_at":"date","user":null},{"id":15,"state":"PENDING","submitted_at":null}]]' ;;
 */issues/1/comments*) echo '[[{"id":21,"updated_at":"date","user":{"login":"self","type":"User"}},{"id":22,"updated_at":"date","user":{"login":"newbot","type":"Bot"}}]]' ;;
 */pulls/1/comments*) echo '[[{"id":23,"updated_at":"date","user":null}]]' ;;
 */check-runs*) echo '[{"check_runs":[]}]' ;;
 */statuses*) echo '[[]]' ;;
 *) exit 1 ;; esac`)
			poll(db, c)
			poll(db, c)
			for _, key := range []string{"review:11:date", "review:12:date", "review:13:date", "review:14:date", "comment:21:date", "comment:22:date", "comment:23:date"} {
				var count int
				if err = db.QueryRow("SELECT count(*) FROM inbox_items WHERE event=?", key).Scan(&count); err != nil || count != 1 {
					t.Errorf("%s count=%d err=%v", key, count, err)
				}
			}
			for _, key := range []string{"review:15:date", "comment:24:date"} {
				if already(db, r.URL, key) {
					t.Errorf("draft was handled: %s", key)
				}
			}
		})
	}
}
