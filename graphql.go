package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const batchSize = 5

var rateLimitedUntil time.Time
var rateFailures int
var errGraphQLRejected = errors.New("GitHub GraphQL batch rejected")
var errGraphQLUnsupported = errors.New("GitHub GraphQL schema unsupported")

type gqlActor struct {
	Login string `json:"login"`
	Type  string `json:"__typename"`
}

func (a *gqlActor) rest() actor {
	if a == nil {
		return actor{}
	}
	name := a.Login
	if a.Type == "Bot" && !strings.HasSuffix(name, "[bot]") {
		name += "[bot]"
	}
	return actor{Login: name, Type: a.Type}
}

type gqlReview struct {
	ID          json.Number `json:"fullDatabaseId"`
	State       string      `json:"state"`
	SubmittedAt *string     `json:"submittedAt"`
	Body        string      `json:"body"`
	Author      *gqlActor   `json:"author"`
}
type gqlComment struct {
	State     string      `json:"state"`
	ID        json.Number `json:"fullDatabaseId"`
	Body      string      `json:"body"`
	UpdatedAt string      `json:"updatedAt"`
	URL       string      `json:"url"`
	Author    *gqlActor   `json:"author"`
	Review    *struct {
		ID json.Number `json:"fullDatabaseId"`
	} `json:"pullRequestReview"`
}
type pageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
}
type gqlSnapshot struct {
	State    string `json:"state"`
	ClosedAt string `json:"closedAt"`
	MergedAt string `json:"mergedAt"`
	Head     string `json:"headRefOid"`
	Reviews  struct {
		Nodes []gqlReview `json:"nodes"`
		Page  *pageInfo   `json:"pageInfo"`
	} `json:"reviews"`
	Comments struct {
		Nodes []gqlComment `json:"nodes"`
		Page  *pageInfo    `json:"pageInfo"`
	} `json:"comments"`
	Threads struct {
		Nodes []struct {
			Comments struct {
				Nodes []gqlComment `json:"nodes"`
				Page  *pageInfo    `json:"pageInfo"`
			} `json:"comments"`
		} `json:"nodes"`
		Page *pageInfo `json:"pageInfo"`
	} `json:"reviewThreads"`
}

func ghEnv() []string {
	t := os.Getenv("GITHUB_TOKEN")
	if t == "" {
		return nil
	}
	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GH_TOKEN=") {
			env = append(env, v)
		}
	}
	return append(env, "GH_TOKEN="+t)
}
func parseRetry(h http.Header, now time.Time) time.Time {
	if v := h.Get("Retry-After"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			return now.Add(time.Duration(n) * time.Second)
		}
		if t, e := http.ParseTime(v); e == nil && t.After(now) {
			return t
		}
	}
	if h.Get("X-Ratelimit-Remaining") == "0" {
		if n, e := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); e == nil && time.Unix(n, 0).After(now) {
			return time.Unix(n, 0)
		}
	}
	return now.Add(time.Minute)
}
func splitGHResponse(out []byte) (http.Header, []byte, int) {
	h := http.Header{}
	status := 0
	if !bytes.HasPrefix(out, []byte("HTTP/")) {
		return h, out, status
	}
	at := bytes.Index(out, []byte("\r\n\r\n"))
	if at < 0 {
		return h, out, status
	}
	lines := strings.Split(string(out[:at]), "\n")
	first := strings.Fields(lines[0])
	if len(first) > 1 {
		status, _ = strconv.Atoi(first[1])
	}
	for _, line := range lines[1:] {
		parts := strings.SplitN(strings.TrimSuffix(line, "\r"), ":", 2)
		if len(parts) == 2 {
			h.Add(textproto.CanonicalMIMEHeaderKey(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
	return h, out[at+4:], status
}

// gh prints headers before each paginated response. Slurp cannot be combined with
// --include because gh interleaves those headers into its JSON array.
func splitGHPages(out []byte) (http.Header, []byte, int, error) {
	out = bytes.TrimSpace(out)
	if !bytes.HasPrefix(out, []byte("HTTP/")) {
		return http.Header{}, out, 0, nil
	}
	var pages []json.RawMessage
	h := http.Header{}
	status := 0
	for len(out) > 0 {
		if !bytes.HasPrefix(out, []byte("HTTP/")) {
			return nil, nil, 0, errors.New("invalid paginated response")
		}
		var body []byte
		h, body, status = splitGHResponse(out)
		if bytes.Equal(body, out) {
			return nil, nil, 0, errors.New("invalid response headers")
		}
		if isRateLimited(h, status, "") {
			return h, nil, status, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		var page json.RawMessage
		if err := decoder.Decode(&page); err != nil {
			return nil, nil, 0, errors.New("invalid paginated response body")
		}
		pages = append(pages, page)
		out = bytes.TrimSpace(body[decoder.InputOffset():])
	}
	body, err := json.Marshal(pages)
	return h, body, status, err
}

func isRateLimited(h http.Header, status int, stderr string) bool {
	return h.Get("X-Ratelimit-Remaining") == "0" || h.Get("Retry-After") != "" || status == 429 || strings.Contains(strings.ToLower(stderr), "rate limit")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func noteRateLimit(h http.Header) {
	now := time.Now()
	until := parseRetry(h, now)
	if h.Get("Retry-After") == "" && h.Get("X-Ratelimit-Remaining") != "0" {
		minutes := 1 << minInt(rateFailures, 4)
		until = now.Add(time.Duration(minutes) * time.Minute)
	}
	rateFailures++
	rateLimitedUntil = until
}

func githubAvailable() bool { return !time.Now().Before(rateLimitedUntil) }
func gqlQuery(rs []registration) (string, error) {
	var b strings.Builder
	b.WriteString("query Monitor {\n")
	for i, r := range rs {
		_, m, e := validURL(r.URL)
		if e != nil {
			return "", e
		}
		n, e := strconv.Atoi(m[3])
		if e != nil {
			return "", e
		}
		fmt.Fprintf(&b, "p%d: repository(owner:%s,name:%s) { pullRequest(number:%d) {\n", i, strconv.Quote(m[1]), strconv.Quote(m[2]), n)
		b.WriteString(`state closedAt mergedAt headRefOid
reviews(first:25) { nodes { fullDatabaseId state submittedAt body author { login __typename } } pageInfo { hasNextPage } }
comments(first:25) { nodes { fullDatabaseId body updatedAt url author { login __typename } } pageInfo { hasNextPage } }
reviewThreads(first:20) { nodes { comments(first:25) { nodes { fullDatabaseId state body updatedAt url pullRequestReview { fullDatabaseId } author { login __typename } } pageInfo { hasNextPage } } } pageInfo { hasNextPage } }
} }
`)
	}
	b.WriteString("rateLimit { cost remaining resetAt }\n}")
	return b.String(), nil
}

type gqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Path    []any  `json:"path"`
	} `json:"errors"`
}
type snapshotResult struct {
	snap *gqlSnapshot
	err  error
}

func ghGraphQL(rs []registration) ([]snapshotResult, error) {
	if !githubAvailable() {
		return nil, errors.New("GitHub rate limit backoff active")
	}
	query, e := gqlQuery(rs)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "--include", "graphql", "-f", "query="+query)
	cmd.Env = ghEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	header, body, status := splitGHResponse(out)
	var resp gqlResponse
	parseErr := json.Unmarshal(body, &resp)
	limited := isRateLimited(header, status, stderr.String())
	for _, v := range resp.Errors {
		if v.Type == "RATE_LIMITED" || strings.Contains(strings.ToLower(v.Message), "rate limit") {
			limited = true
		}
	}
	if limited {
		noteRateLimit(header)
		return nil, errors.New("GitHub rate limited the monitor; backing off")
	}
	if parseErr != nil {
		return nil, fmt.Errorf("invalid GraphQL response: %w", parseErr)
	}
	// gh exits nonzero for a GraphQL response containing errors, including partial data.
	if e != nil && len(resp.Errors) == 0 {
		return nil, errors.New("GraphQL request failed")
	}
	results := make([]snapshotResult, len(rs))
	globalErr := false
	unsupported := false
	for _, v := range resp.Errors {
		if strings.Contains(v.Message, "doesn't exist on type") || strings.Contains(v.Message, "Cannot query field") {
			unsupported = true
		}
		if len(v.Path) == 0 {
			globalErr = true
			continue
		}
		alias, ok := v.Path[0].(string)
		if !ok || !strings.HasPrefix(alias, "p") {
			globalErr = true
			continue
		}
		i, err := strconv.Atoi(alias[1:])
		if err != nil || i < 0 || i >= len(results) {
			globalErr = true
			continue
		}
		results[i].err = errors.New("GitHub returned a partial GraphQL result")
	}
	if globalErr {
		if unsupported {
			return nil, errGraphQLUnsupported
		}
		return nil, errGraphQLRejected
	}
	for i := range results {
		if results[i].err != nil {
			continue
		}
		raw := resp.Data[fmt.Sprintf("p%d", i)]
		var v struct {
			Pull *gqlSnapshot `json:"pullRequest"`
		}
		if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &v) != nil || v.Pull == nil {
			results[i].err = errors.New("PR unavailable in GraphQL response")
			continue
		}
		results[i].snap = v.Pull
	}
	return results, nil
}
func (s *gqlSnapshot) restReviews() ([][]apiReview, bool) {
	if s.Reviews.Page == nil || s.Reviews.Nodes == nil || s.Reviews.Page.HasNextPage {
		return nil, false
	}
	var out []apiReview
	for _, v := range s.Reviews.Nodes {
		id, e := v.ID.Int64()
		if e != nil || id <= 0 {
			return nil, false
		}
		out = append(out, apiReview{ID: id, State: v.State, SubmittedAt: v.SubmittedAt, Body: v.Body, User: v.Author.rest()})
	}
	return [][]apiReview{out}, true
}
func (s *gqlSnapshot) restComments(inline bool) ([][]apiComment, bool) {
	var nodes []gqlComment
	if !inline {
		if s.Comments.Page == nil || s.Comments.Nodes == nil || s.Comments.Page.HasNextPage {
			return nil, false
		}
		nodes = s.Comments.Nodes
	} else {
		if s.Threads.Page == nil || s.Threads.Nodes == nil || s.Threads.Page.HasNextPage {
			return nil, false
		}
		for _, t := range s.Threads.Nodes {
			if t.Comments.Page == nil || t.Comments.Nodes == nil || t.Comments.Page.HasNextPage {
				return nil, false
			}
			nodes = append(nodes, t.Comments.Nodes...)
		}
	}
	var out []apiComment
	for _, v := range nodes {
		if inline && v.State == "PENDING" {
			continue
		}
		id, e := v.ID.Int64()
		if e != nil || id <= 0 {
			return nil, false
		}
		var reviewID int64
		if v.Review != nil {
			reviewID, e = v.Review.ID.Int64()
			if e != nil {
				return nil, false
			}
		}
		out = append(out, apiComment{ID: id, User: v.Author.rest(), Body: v.Body, UpdatedAt: v.UpdatedAt, HTMLURL: v.URL, ReviewID: reviewID})
	}
	return [][]apiComment{out}, true
}
