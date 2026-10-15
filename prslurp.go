// pslurp.go — extract unresolved code-review threads from a GitHub PR
// into a Markdown file for consumption by an LLM.
//
// It shells out to the `gh` CLI (GraphQL API) because the REST API does not
// expose whether a review thread is resolved — only GraphQL
// (PullRequest.reviewThreads.isResolved) does.
//
// Requirements:
//   - gh CLI installed and authenticated to the repo's host
//     (gh auth login — works with SSH or HTTPS credentials).
//   - The repo must be readable by the authenticated user.
//
// Build (stdlib only, no dependencies):
//
	go build pslurp.go
//
// Usage (flags may come before or after the PR argument):
//
	pslurp https://github.com/acme/widgets/pull/482
	pslurp https://github.com/acme/widgets/pull/482 -o feedback.md
	pslurp -repo acme/widgets 482
	pslurp acme/widgets#482 -incl-resolved | head -100
//
// Flags:
//
//	-repo owner/repo   repository containing the PR (not needed when the
//	                   positional arg is a URL or "owner/repo#N")
//	-incl-resolved     also include threads that were resolved (tagged as such)
//	-incl-outdated     also include threads whose diff position is outdated
//	                   (code has moved on; excluded by default)
//	-skip-collapsed    skip threads that have replies you haven't viewed yet
//	                   (GitHub marks these "collapsed"; included by default)
//	-o path            write output here (default:
//	                   <owner>-<repo>-pr<number>-unresolved-feedback.md in the
//	                   current directory; use "-" for stdout)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	threadsPerPage = 50
	maxPages       = 100 // 5000 threads; more than enough, guards against infinite loops
)

// Query paginates over reviewThreads($cursor). `first: 100` on comments is a
// safety cap; if a thread somehow has more comments we truncate and say so.
// PENDING review-level bodies are fetched too — reviewers occasionally put
// context there rather than in inline comments.
const queryTemplate = `query ReviewThreads($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      title
      url
      state
      isDraft
      author { login }
      baseRefName
      headRefName
      headRefOid
      reviewDecision
      createdAt
      updatedAt
      reviews(states: [PENDING], first: 10) {
        nodes {
          createdAt
          author { login }
          body
        }
      }
      reviewThreads(first: %d, after: $cursor) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes {
          isResolved
          isOutdated
          isCollapsed
          path
          startLine
          line
          comments(first: 100) {
            totalCount
            nodes {
              createdAt
              url
              author { login }
              body
            }
          }
        }
      }
    }
  }
}`

// ---------- JSON shapes (only what we need) ----------

type actor struct {
	Login string `json:"login"`
}

type commentNode struct {
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
	Author    *actor `json:"author"`
	Body      string `json:"body"`
}

func (c commentNode) deepCopy() commentNode {
	if c.Author != nil {
		a := *c.Author
		c.Author = &a
	}
	return c
}

// deepCopy returns a standalone copy of t that shares no slice backing arrays
// with its source. Needed because json.Unmarshal reuses nested slice capacity
// when decoding into a reused struct, so accumulated copies could be
// overwritten by later pages.
func (t threadNode) deepCopy() threadNode {
	t.Comments.Nodes = append([]commentNode(nil), t.Comments.Nodes...)
	for i := range t.Comments.Nodes {
		t.Comments.Nodes[i] = t.Comments.Nodes[i].deepCopy()
	}
	return t
}

type threadNode struct {
	IsResolved  bool   `json:"isResolved"`
	IsOutdated  bool   `json:"isOutdated"`
	IsCollapsed bool   `json:"isCollapsed"`
	Path        string `json:"path"`
	StartLine   *int   `json:"startLine"`
	Line        *int   `json:"line"`
	Comments    struct {
		TotalCount int           `json:"totalCount"`
		Nodes      []commentNode `json:"nodes"`
	} `json:"comments"`
}

type gqlResponse struct {
	Data struct {
		Repository struct {
			PullRequest *struct {
				Title          string  `json:"title"`
				URL            string  `json:"url"`
				State          string  `json:"state"`
				IsDraft        bool    `json:"isDraft"`
				Author         *actor  `json:"author"`
				BaseRefName    string  `json:"baseRefName"`
				HeadRefName    string  `json:"headRefName"`
				HeadRefOid     string  `json:"headRefOid"`
				ReviewDecision *string `json:"reviewDecision"`
				CreatedAt      string  `json:"createdAt"`
				UpdatedAt      string  `json:"updatedAt"`
				Reviews        struct {
					Nodes []struct {
						CreatedAt string `json:"createdAt"`
						Author    *actor `json:"author"`
						Body      string `json:"body"`
					} `json:"nodes"`
				} `json:"reviews"`
				ReviewThreads struct {
					TotalCount int `json:"totalCount"`
					PageInfo   struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []threadNode `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// ---------- CLI arg parsing ----------

type options struct {
	repoArg       string
	inclResolved  bool
	inclOutdated  bool
	skipCollapsed bool
	outPath       string
	positional    string
	owner         string
	repo          string
	number        int
}

// parseFlags collects flags from anywhere in argv (before or after the PR
// argument). Go's flag package stops at the first non-flag token, which would
// make `pr-unresolved <URL> -o out.md` fail; this doesn't.
func parseFlags() (*options, error) {
	o := &options{}
	args := os.Args[1:]
	rest := []string{}
	help := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case a == "-h" || a == "-help" || a == "--help":
			help = true
		case strings.HasPrefix(a, "-o="):
			o.outPath = strings.TrimPrefix(a, "-o=")
		case a == "-o" || a == "--o":
			if i+1 >= len(args) {
				return nil, errors.New("flag -o requires a value")
			}
			i++
			o.outPath = args[i]
		case strings.HasPrefix(a, "-repo="):
			o.repoArg = strings.TrimPrefix(a, "-repo=")
		case a == "-repo" || a == "--repo":
			if i+1 >= len(args) {
				return nil, errors.New("flag -repo requires a value")
			}
			i++
			o.repoArg = args[i]
		case a == "-incl-resolved" || a == "--incl-resolved":
			o.inclResolved = true
		case a == "-incl-outdated" || a == "--incl-outdated":
			o.inclOutdated = true
		case a == "-skip-collapsed" || a == "--skip-collapsed":
			o.skipCollapsed = true
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %q", a)
		default:
			rest = append(rest, a)
		}
	}

	if help {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(0)
	}
	if len(rest) != 1 {
		return nil, errors.New("expected exactly one argument: a PR URL, \"owner/repo#N\", or a bare PR number (with -repo)")
	}
	o.positional = rest[0]
	return o, nil
}

const usage = `usage: pr-unresolved [flags] <PR URL | owner/repo#N | N>

Extract unresolved code-review threads from a GitHub PR into a Markdown file
for consumption by an LLM. Uses the gh CLI (GraphQL API).

flags:
  -repo owner/repo      repository containing the PR (needed only for a bare PR number)
  -incl-resolved        also include threads that were resolved
  -incl-outdated        also include threads whose diff position is outdated
  -skip-collapsed       skip threads with replies you haven't viewed yet
  -o path               output file (default: <owner>-<repo>-pr<N>-unresolved-feedback.md;
                        "-" writes to stdout)

examples:
  pr-unresolved https://github.com/acme/widgets/pull/482
  pr-unresolved https://github.com/acme/widgets/pull/482 -o feedback.md
  pr-unresolved -repo acme/widgets 482
  pr-unresolved -incl-resolved acme/widgets#482 | head -100`

var ghURLRe = regexp.MustCompile(`^https?://[^/]+/([^/]+)/([^/]+)/pull/(\d+)`)
var numOnlyRe = regexp.MustCompile(`^(\d+)$`)

// resolvePR derives owner/repo/number from flags + positional arg.
// Accepted forms:
//
//	https://github.com/OWNER/REPO/pull/123[/files][#discussion_r…]
//	OWNER/REPO#123
//	123                        (requires -repo OWNER/REPO)
func (o *options) resolvePR() error {
	p := o.positional
	if m := ghURLRe.FindStringSubmatch(p); m != nil {
		n, err := strconv.Atoi(m[3])
		if err != nil {
			return fmt.Errorf("bad PR number in %q", p)
		}
		if o.repoArg != "" && o.repoArg != m[1]+"/"+m[2] {
			return fmt.Errorf("-repo %q conflicts with URL %q", o.repoArg, p)
		}
		o.owner, o.repo, o.number = m[1], m[2], n
		return nil
	}
	if strings.Contains(p, "/") && strings.Contains(p, "#") {
		parts := strings.SplitN(p, "#", 2)
		slash := strings.Split(parts[0], "/")
		if len(slash) != 2 {
			return fmt.Errorf("cannot parse %q: want OWNER/REPO#N", p)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("cannot parse PR number in %q", p)
		}
		if o.repoArg != "" && o.repoArg != parts[0] {
			return fmt.Errorf("-repo %q conflicts with %q", o.repoArg, p)
		}
		o.owner, o.repo, o.number = slash[0], slash[1], n
		return nil
	}
	if m := numOnlyRe.FindStringSubmatch(p); m != nil {
		if !strings.Contains(o.repoArg, "/") {
			return errors.New("a bare PR number requires -repo OWNER/REPO")
		}
		parts := strings.SplitN(o.repoArg, "/", 2)
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return fmt.Errorf("bad PR number %q", p)
		}
		o.owner, o.repo, o.number = parts[0], parts[1], n
		return nil
	}
	return fmt.Errorf("cannot parse %q: want a PR URL, OWNER/REPO#N, or a number with -repo", p)
}

// ---------- gh interaction ----------

// runGQL executes one GraphQL page via `gh api graphql`, with the query body
// piped through stdin as the `query` variable.
func runGQL(query string, owner, repo string, number int, cursor string) ([]byte, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, errors.New("the `gh` CLI was not found on PATH — install it (https://cli.github.com) and run `gh auth login`")
	}
	cursorArg := "null"
	if cursor != "" {
		cursorArg = cursor
	}
	args := []string{
		"api", "graphql",
		"-F", "owner=" + owner,
		"-F", "name=" + repo,
		"-F", "number=" + strconv.Itoa(number),
		"-F", "cursor=" + cursorArg,
		"-F", "query=@-",
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = strings.NewReader(query)

	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gh api graphql failed: %s\n(hint: is gh installed and authenticated? `gh auth login` — SSH or HTTPS both work)", msg)
	}
	return []byte(out.String()), nil
}

// fetchThreads pages through reviewThreads and returns everything the GraphQL
// API reports, plus PR metadata and pending review bodies.
//
// Each page is decoded into a FRESH gqlResponse: reusing one struct across
// pages makes json.Unmarshal reuse the nested slice backing arrays, so later
// pages silently overwrite thread data that accumulated page copies point at.
func fetchThreads(o *options, query string) (*gqlResponse, []threadNode, error) {
	var all []threadNode
	latest := &gqlResponse{} // PR metadata comes from the last fetched page

	for page := 1; page <= maxPages; page++ {
		cursor := ""
		if page > 1 {
			cursor = latest.Data.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
		}
		raw, err := runGQL(query, o.owner, o.repo, o.number, cursor)
		if err != nil {
			return nil, nil, err
		}
		var pageResp gqlResponse
		if err := json.Unmarshal(raw, &pageResp); err != nil {
			return nil, nil, fmt.Errorf("cannot decode gh output (is it JSON?): %w\n%s", err, truncate(string(raw), 600))
		}
		pr := pageResp.Data.Repository.PullRequest
		if pr == nil {
			return nil, nil, fmt.Errorf("PR not found or not readable: %s/%s#%d (check the URL and your gh permissions)",
				o.owner, o.repo, o.number)
		}
		latest = &pageResp

		// Deep-copy this page's threads; do not retain slices that alias pageResp
		// (they'd be reused by json.Unmarshal on the next page).
		pageThreads := pr.ReviewThreads.Nodes
		pageThreads = append([]threadNode(nil), pageThreads...)
		for i := range pageThreads {
			pageThreads[i] = pageThreads[i].deepCopy()
		}
		all = append(all, pageThreads...)

		fmt.Fprintf(os.Stderr, "  fetched thread page %d (%d threads so far of %d)\n",
			page, len(all), pr.ReviewThreads.TotalCount)
		if !pr.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		if page == maxPages {
			fmt.Fprintf(os.Stderr, "  warning: stopped after %d pages; results may be truncated\n", maxPages)
		}
	}
	return latest, all, nil
}

// ---------- filtering / sorting ----------

func keepThread(o *options, t threadNode) bool {
	if t.IsResolved && !o.inclResolved {
		return false
	}
	if t.IsOutdated && !o.inclOutdated {
		return false
	}
	if t.IsCollapsed && o.skipCollapsed {
		return false
	}
	return len(t.Comments.Nodes) > 0
}

func sortThreads(ts []threadNode) {
	lineOf := func(t threadNode) int {
		if n, ok := primaryLine(t); ok {
			return n
		}
		return 0
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Path != ts[j].Path {
			return ts[i].Path < ts[j].Path
		}
		a := lineOf(ts[i])
		b := lineOf(ts[j])
		if a != b {
			return a < b
		}
		return ts[i].Comments.Nodes[0].CreatedAt < ts[j].Comments.Nodes[0].CreatedAt
	})
}

// ---------- rendering ----------

func primaryLine(t threadNode) (int, bool) {
	if t.Line != nil {
		return *t.Line, true
	}
	if t.StartLine != nil {
		return *t.StartLine, true
	}
	return 0, false
}

func lineRange(t threadNode) string {
	if t.StartLine != nil && t.Line != nil && *t.StartLine != *t.Line {
		return fmt.Sprintf(":%d-%d", *t.StartLine, *t.Line)
	}
	if n, ok := primaryLine(t); ok {
		return fmt.Sprintf(":%d", n)
	}
	return ""
}

func authorName(a *actor) string {
	if a == nil || strings.TrimSpace(a.Login) == "" {
		return "deleted user"
	}
	return "@" + a.Login
}

func fmtTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

// firstLine returns the first non-empty line of the thread's opening comment,
// for sorting and the summary list.
func threadTitle(t threadNode) string {
	for _, c := range t.Comments.Nodes {
		for _, ln := range strings.Split(c.Body, "\n") {
			ln = strings.TrimSpace(strings.TrimPrefix(ln, ">"))
			if ln != "" {
				return truncateRunes(ln, 100)
			}
		}
	}
	return "(no comment text)"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func humanizeState(s string) string {
	switch s {
	case "OPEN":
		return "Open"
	case "CLOSED":
		return "Closed"
	case "MERGED":
		return "Merged"
	}
	return s
}

func humanizeDecision(s string) string {
	switch s {
	case "CHANGES_REQUESTED":
		return "Changes requested"
	case "APPROVED":
		return "Approved"
	case "REVIEW_REQUIRED":
		return "Review required"
	case "DISMISSED":
		return "Review dismissed"
	case "":
		return "None yet"
	}
	return s
}

func renderDoc(o *options, resp *gqlResponse, kept []threadNode, skipped struct{ resolved, outdated, collapsed int }) string {
	pr := resp.Data.Repository.PullRequest
	var b strings.Builder

	b.WriteString("# Unresolved Review Feedback\n\n")
	fmt.Fprintf(&b, "**Repo:** `%s/%s` · **PR:** [#%d](%s) — %s\n\n", o.owner, o.repo, o.number, pr.URL, pr.Title)
	fmt.Fprintf(&b, "- **Author:** %s · **State:** %s%s\n", authorName(pr.Author), humanizeState(pr.State), map[bool]string{true: " · draft", false: ""}[pr.IsDraft])
	fmt.Fprintf(&b, "- **Branches:** `%s` ← `%s` (`%s`)\n", pr.BaseRefName, pr.HeadRefName, shortSHA(pr.HeadRefOid))
	if pr.ReviewDecision != nil {
		fmt.Fprintf(&b, "- **Review decision:** %s\n", humanizeDecision(*pr.ReviewDecision))
	}
	fmt.Fprintf(&b, "- **Unresolved threads:** %d", len(kept))
	var notes []string
	if skipped.resolved > 0 {
		notes = append(notes, fmt.Sprintf("%d resolved excluded", skipped.resolved))
	}
	if skipped.outdated > 0 {
		notes = append(notes, fmt.Sprintf("%d outdated excluded", skipped.outdated))
	}
	if skipped.collapsed > 0 {
		notes = append(notes, fmt.Sprintf("%d with unseen replies excluded", skipped.collapsed))
	}
	if len(notes) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(notes, ", "))
	}
	fmt.Fprintf(&b, "\n- **Generated:** %s (local time)\n\n", time.Now().Format("2006-01-02 15:04"))

	// Summary list — a quick scan for the LLM before the full conversation.
	b.WriteString("## Summary\n\n")
	if len(kept) == 0 {
		b.WriteString("_No unresolved review threads._\n\n")
	} else {
		for i, t := range kept {
			n, ok := primaryLine(t)
			loc := ""
			if ok {
				loc = fmt.Sprintf(":%d", n)
			}
			first := t.Comments.Nodes[0]
			fmt.Fprintf(&b, "- **#%d** `%s%s` — %s: %q\n", i+1, t.Path, loc, authorName(first.Author), threadTitle(t))
		}
		b.WriteString("\n")
	}

	if len(kept) > 0 {
		b.WriteString("## Threads\n\n")
		curPath := ""
		idx := 0
		for _, t := range kept {
			idx++
			if t.Path != curPath {
				curPath = t.Path
				fmt.Fprintf(&b, "### `%s`\n\n", t.Path)
			}

			status := "Unresolved"
			if t.IsResolved {
				status = "Resolved"
			}
			extra := ""
			if t.IsOutdated {
				extra = " · outdated position (code has changed since)"
			}
			if t.IsCollapsed {
				extra += " · has replies you haven't viewed"
			}
			first := t.Comments.Nodes[0]
			fmt.Fprintf(&b, "#### Thread #%d · `%s%s` · %s%s · opened %s by %s\n\n",
				idx, t.Path, lineRange(t), status, extra, fmtTime(first.CreatedAt), authorName(first.Author))

			for ci, c := range t.Comments.Nodes {
				label := "comment"
				if ci == 0 {
					label = "opening comment"
				}
				fmt.Fprintf(&b, "**%s** — %s · [%s](%s)\n\n", authorName(c.Author), fmtTime(c.CreatedAt), label, c.URL)
				body := strings.TrimSpace(c.Body)
				if body == "" {
					body = "_empty comment_"
				}
				b.WriteString(body)
				b.WriteString("\n\n")
			}
			if t.Comments.TotalCount > len(t.Comments.Nodes) {
				fmt.Fprintf(&b, "_(thread has %d comments; only first %d fetched)_\n\n", t.Comments.TotalCount, len(t.Comments.Nodes))
			}
			b.WriteString("---\n\n")
		}
	}

	// Pending (unsubmitted) review bodies: reviewers sometimes leave context there.
	var pendings []string
	for _, r := range pr.Reviews.Nodes {
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		pendings = append(pendings, fmt.Sprintf("**%s** — created %s\n\n%s", authorName(r.Author), fmtTime(r.CreatedAt), strings.TrimSpace(r.Body)))
	}
	if len(pendings) > 0 {
		b.WriteString("## Pending (unsubmitted) review summaries\n\n")
		for _, p := range pendings {
			b.WriteString(p)
			b.WriteString("\n\n---\n\n")
		}
	}

	fmt.Fprintf(&b, "_Source: GitHub GraphQL API via `gh api graphql`._\n")
	return b.String()
}

func shortSHA(s string) string {
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// ---------- main ----------

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	o, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "run 'pr-unresolved -h' for usage")
		return errors.New("bad arguments")
	}
	if err := o.resolvePR(); err != nil {
		return err
	}

	query := fmt.Sprintf(queryTemplate, threadsPerPage)
	resp, threads, err := fetchThreads(o, query)
	if err != nil {
		return err
	}

	kept := threads[:0]
	var skipped struct{ resolved, outdated, collapsed int }
	for _, t := range threads {
		switch {
		case t.IsResolved && !o.inclResolved:
			skipped.resolved++
		case t.IsOutdated && !o.inclOutdated:
			skipped.outdated++
		case t.IsCollapsed && o.skipCollapsed:
			skipped.collapsed++
		case !keepThread(o, t):
			// zero-comment thread; ignore silently
		default:
			kept = append(kept, t)
		}
	}
	sortThreads(kept)

	doc := renderDoc(o, resp, kept, skipped)

	// Output: explicit -o, or a default file, or stdout with -o -.
	out := o.outPath
	if out == "" {
		out = fmt.Sprintf("%s-%s-pr%d-unresolved-feedback.md", o.owner, o.repo, o.number)
	}
	if out == "-" {
		fmt.Print(doc)
		fmt.Fprintf(os.Stderr, "wrote %d unresolved thread(s) to stdout\n", len(kept))
		return nil
	}
	if err := os.WriteFile(out, []byte(doc), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", out, err)
	}
	abs, _ := filepath.Abs(out)
	fmt.Fprintf(os.Stderr, "wrote %d unresolved thread(s) across %d file(s) to %s\n", len(kept), countFiles(kept), abs)
	return nil
}

func countFiles(ts []threadNode) int {
	seen := map[string]bool{}
	for _, t := range ts {
		seen[t.Path] = true
	}
	return len(seen)
}
