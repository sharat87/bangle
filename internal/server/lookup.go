package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Looking up a remote server's MCP URL by name (GET /api/lookup?name=linear),
// for the Add page.
//
// Context7 (context7.com) indexes vendors' docs. Its search takes a question
// and answers with documentation snippets, never with one answer: it ignores
// "reply with just the URL". So Bangle asks two phrasings of the question,
// pulls every https URL out of the snippets, and decides itself:
//
//  1. A candidate mentions "mcp", has no query or fragment, and is not an
//     /sse endpoint (Bangle speaks streamable HTTP only).
//     The name asked about (its first word: "wordpress" for "wordpress
//     com") must be in it or in a docs page it came from, which drops the
//     MCP spec's own example servers.
//  2. It is "verified" when its host is on the same domain as a docs page it
//     came from (mcp.linear.app from linear.app/docs/mcp). That is the
//     signal that it's the vendor's own server and not a third-party proxy.
//  3. The best few are checked with discover, the same unauthenticated
//     request and OAuth discovery Connect runs. Only the ones that pass are
//     returned: a URL from the docs that isn't an OAuth MCP server is
//     dropped, so a wrong guess never reaches the form.
//
// The result only prefills the form. Nothing is added without me reading
// the URL and its source and pressing Add.

const (
	context7SearchURL = "https://context7.com/api/v3/search"
	// lookupTimeout bounds the whole lookup: both searches and every check.
	lookupTimeout = 30 * time.Second
	// maxLookupChecks caps how many candidates are checked with discover.
	maxLookupChecks = 6
	maxContext7Body = 2 << 20
)

// lookupQueries are the questions sent to Context7, tuned against Linear,
// Notion, PostHog, Railway, Stripe, Sentry and GitHub. The first finds the
// vendor's setup page for most; the second finds Sentry's and GitHub's.
var lookupQueries = []string{
	"%s remote MCP server URL",
	"How do I connect to the official %s MCP server? What is its URL?",
}

type lookupCandidate struct {
	URL string `json:"url"`
	// Sources are the docs pages that mention it, best first.
	Sources []string `json:"sources"`
	// Verified: on the same domain as one of its sources.
	Verified bool `json:"verified"`
	Mentions int  `json:"mentions"`
}

type lookupResult struct {
	Candidates []lookupCandidate `json:"candidates"`
	// Checked is how many URLs from the docs were tried with discover.
	Checked int `json:"checked"`
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// CONTEXT7_API_KEY is optional; /api/health tells the UI whether to
	// offer this, so a request without it is a mistake, said plainly.
	if s.context7Key == "" {
		writeJSONError(w, http.StatusNotFound, "finding a URL by name is off: CONTEXT7_API_KEY is not set")
		return
	}
	// Server names are a-z, 0-9 and -: "wordpress-com" asks about
	// "wordpress com".
	name := strings.Join(strings.FieldsFunc(r.URL.Query().Get("name"), func(c rune) bool {
		return c == '-' || c == ' '
	}), " ")
	if name == "" || len(name) > 64 {
		writeJSONError(w, http.StatusBadRequest, "name is required, at most 64 characters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), lookupTimeout)
	defer cancel()
	res, err := s.lookup(ctx, name)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) lookup(ctx context.Context, name string) (lookupResult, error) {
	snippets := make([][]context7Snippet, len(lookupQueries))
	errs := make([]error, len(lookupQueries))
	var wg sync.WaitGroup
	for i, q := range lookupQueries {
		wg.Go(func() { snippets[i], errs[i] = s.searchContext7(ctx, fmt.Sprintf(q, name)) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return lookupResult{}, err
		}
	}
	candidates := slices.DeleteFunc(lookupCandidates(slices.Concat(snippets...)), func(c lookupCandidate) bool {
		return !mentionsName(c, name)
	})
	candidates = candidates[:min(len(candidates), maxLookupChecks)]

	ok := make([]bool, len(candidates))
	for i, c := range candidates {
		wg.Go(func() {
			_, err := discover(ctx, s.upstream, c.URL)
			ok[i] = err == nil
		})
	}
	wg.Wait()
	out := lookupResult{Candidates: []lookupCandidate{}, Checked: len(candidates)}
	for i, c := range candidates {
		if ok[i] {
			out.Candidates = append(out.Candidates, c)
		}
	}
	return out, nil
}

// mentionsName reports whether the first word of the name is in the
// candidate's URL or one of its sources: Context7 answers "atlassian" with
// the MCP spec's example-server.modelcontextprotocol.io too.
func mentionsName(c lookupCandidate, name string) bool {
	word := strings.ToLower(strings.Fields(name)[0])
	for _, u := range append([]string{c.URL}, c.Sources...) {
		if strings.Contains(strings.ToLower(u), word) {
			return true
		}
	}
	return false
}

// context7Snippet is one snippet of a search's answer: the docs page it's
// from, and its text.
type context7Snippet struct {
	Source string
	Text   string
}

func (s *Server) searchContext7(ctx context.Context, query string) ([]context7Snippet, error) {
	u := s.context7URL + "?" + url.Values{"query": {query}, "type": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.context7Key)
	resp, err := s.upstream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching Context7: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxContext7Body))
	if err != nil {
		return nil, fmt.Errorf("reading Context7's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Context7 answered HTTP %d: %.200s", resp.StatusCode, body)
	}
	var parsed struct {
		CodeSnippets []struct {
			CodeID          string `json:"codeId"`
			CodeTitle       string `json:"codeTitle"`
			CodeDescription string `json:"codeDescription"`
			CodeList        []struct {
				Code string `json:"code"`
			} `json:"codeList"`
		} `json:"codeSnippets"`
		InfoSnippets []struct {
			PageID  string `json:"pageId"`
			Content string `json:"content"`
		} `json:"infoSnippets"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("Context7's answer is not the expected JSON: %w", err)
	}
	var out []context7Snippet
	for _, c := range parsed.CodeSnippets {
		parts := []string{c.CodeTitle, c.CodeDescription}
		for _, code := range c.CodeList {
			parts = append(parts, code.Code)
		}
		out = append(out, context7Snippet{Source: c.CodeID, Text: strings.Join(parts, "\n")})
	}
	for _, i := range parsed.InfoSnippets {
		out = append(out, context7Snippet{Source: i.PageID, Text: i.Content})
	}
	return out, nil
}

// snippetURL is an https URL in prose, markdown or code: it ends at
// whitespace, a quote, a backtick or a bracket.
var snippetURL = regexp.MustCompile("https://[^\\s\"'`<>()\\[\\]{},\\\\]+")

// lookupCandidates picks the URLs that could be MCP endpoints out of the
// snippets, verified first, then by how many snippets mention them. URLs
// differing only in a trailing slash are one candidate, in its most
// mentioned spelling.
func lookupCandidates(snippets []context7Snippet) []lookupCandidate {
	type entry struct {
		spellings map[string]int
		sources   []string
		verified  bool
		mentions  int
	}
	byKey := map[string]*entry{}
	var keys []string
	for _, sn := range snippets {
		seen := map[string]bool{}
		for _, raw := range snippetURL.FindAllString(sn.Text, -1) {
			raw = strings.TrimRight(raw, ".:;!?*")
			u, err := url.Parse(raw)
			if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || u.User != nil {
				continue
			}
			if !strings.Contains(strings.ToLower(raw), "mcp") || strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/sse") {
				continue
			}
			key := strings.TrimSuffix(raw, "/")
			e := byKey[key]
			if e == nil {
				e = &entry{spellings: map[string]int{}}
				byKey[key] = e
				keys = append(keys, key)
			}
			e.spellings[raw]++
			if seen[key] {
				continue
			}
			seen[key] = true
			e.mentions++
			if !slices.Contains(e.sources, sn.Source) {
				e.sources = append(e.sources, sn.Source)
			}
			if src, err := url.Parse(sn.Source); err == nil && sameSite(u.Hostname(), src.Hostname()) {
				e.verified = true
			}
		}
	}
	out := make([]lookupCandidate, 0, len(keys))
	for _, k := range keys {
		e := byKey[k]
		best, n := "", 0
		for sp, c := range e.spellings {
			if c > n || (c == n && sp < best) {
				best, n = sp, c
			}
		}
		out = append(out, lookupCandidate{URL: best, Sources: e.sources, Verified: e.verified, Mentions: e.mentions})
	}
	// Stable: equal candidates keep the order Context7 ranked them in.
	slices.SortStableFunc(out, func(a, b lookupCandidate) int {
		if a.Verified != b.Verified {
			if a.Verified {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.Mentions, a.Mentions)
	})
	return out
}

// sameSite reports whether two hosts share a registrable domain:
// mcp.linear.app and linear.app do, api.githubcopilot.com and github.com
// don't. Without a public suffix list it takes the last two labels, or three
// under a two-letter country code's co/com/org/net/ac/gov/edu (example.co.uk).
// It only labels a candidate as verified for me to read; it gates nothing.
func sameSite(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return registrableDomain(a) == registrableDomain(b)
}

func registrableDomain(host string) string {
	labels := strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".")
	n := 2
	if len(labels) > 2 && len(labels[len(labels)-1]) == 2 &&
		slices.Contains([]string{"co", "com", "org", "net", "ac", "gov", "edu"}, labels[len(labels)-2]) {
		n = 3
	}
	return strings.Join(labels[max(0, len(labels)-n):], ".")
}
