package research

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// MaxQueryRunes caps a search query at a sentence, not a paragraph.
const MaxQueryRunes = 160

// maxURLBytes caps a URL given to web_fetch.
const maxURLBytes = 2048

// The rules a refusal names.
const (
	RuleLength      = "length"
	RuleCode        = "code"
	RulePath        = "path"
	RuleFile        = "file"
	RuleIdentifier  = "identifier"
	RuleModule      = "module"
	RuleName        = "name"
	RuleCard        = "card"
	RuleScheme      = "scheme"
	RuleCredentials = "credentials"
	// RuleIndex refuses while the checkouts cannot be read: the guard fails
	// closed.
	RuleIndex = "index"
)

// Terms are the project-specific words the guard refuses.
type Terms struct {
	// Identifiers are the compound identifiers the checkouts declare.
	Identifiers []string
	// Modules are the module paths and package names their root manifests
	// name.
	Modules []string
	// Names are the target projects' names and their repositories' owner
	// and repository names.
	Names []string
	// CardPrefixes are the card id prefixes of ContextMatrix's projects.
	CardPrefixes []string
}

// Refusal is the guard refusing a query or a URL: the rule, the token that
// broke it, and why, in words the model can act on.
type Refusal struct {
	Rule   string
	Token  string
	Reason string
}

func (r *Refusal) Error() string {
	if r.Token == "" {
		return fmt.Sprintf("refused by the leak guard (rule %s): %s", r.Rule, r.Reason)
	}

	return fmt.Sprintf("refused by the leak guard (rule %s): %q %s", r.Rule, r.Token, r.Reason)
}

// Guard refuses code-shaped and project-specific text in what a stage sends
// out: search queries, and the host, path and query of a URL. Plain words and
// technology names pass. The rules that need no terms run first, so a query
// they refuse never makes the guard read the checkouts.
type Guard struct {
	load func(context.Context) (Terms, error)

	mu     sync.Mutex
	loaded bool
	terms  compiled
}

// NewGuard returns a guard whose terms load reads once, at the first check
// that needs them; a failed read is tried again at the next. A nil load
// means no terms.
func NewGuard(load func(context.Context) (Terms, error)) *Guard {
	return &Guard{load: load}
}

// Query checks a web search query. Every error is a *Refusal.
func (g *Guard) Query(ctx context.Context, q string) error {
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) > MaxQueryRunes {
		return &Refusal{Rule: RuleLength, Reason: fmt.Sprintf("the query is longer than %d characters; search with a sentence, not a paragraph", MaxQueryRunes)}
	}

	tokens := strings.Fields(q)

	for _, tok := range tokens {
		if r := shape(tok, true); r != nil {
			return r
		}
	}

	t, err := g.termsFor(ctx)
	if err != nil {
		return indexRefusal(err)
	}

	for _, tok := range tokens {
		if r := t.refusal(bare(tok)); r != nil {
			return r
		}
	}

	return nil
}

// URL checks a URL web_fetch is given and returns it parsed. It must be
// absolute http or https without credentials, and its path and query must
// pass the query rules: each path segment, query key and whole query value
// obeys the query length cap. Path segments skip the path and file rules,
// which a URL's own syntax breaks, and a name's ( or =, which page names
// carry. Each label of the host gets the term rules only: a host is no
// sentence, but a page can ask for https://<data>.attacker.example/, so a
// label leaks like a path segment.
func (g *Guard) URL(ctx context.Context, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxURLBytes {
		return nil, &Refusal{Rule: RuleLength, Reason: fmt.Sprintf("the URL is longer than %d bytes", maxURLBytes)}
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("%q is not an absolute http or https URL", raw)
	}

	// The scheme is refused before the host is required: file:///etc is
	// absolute but has no host, and is a scheme refusal.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &Refusal{Rule: RuleScheme, Token: u.Scheme, Reason: "is not http or https"}
	}

	if u.Host == "" {
		return nil, fmt.Errorf("%q is not an absolute http or https URL", raw)
	}

	if u.User != nil {
		return nil, &Refusal{Rule: RuleCredentials, Reason: "the URL carries credentials"}
	}

	// Checked as parsed: a pair that does not parse would reach the server
	// unchecked.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("the URL's query does not parse: %w", err)
	}

	var segments, values []string

	for seg := range strings.SplitSeq(u.Path, "/") {
		if utf8.RuneCountInString(seg) > MaxQueryRunes {
			return nil, &Refusal{Rule: RuleLength, Reason: fmt.Sprintf("a path segment of the URL is longer than %d characters", MaxQueryRunes)}
		}

		if seg != "" {
			segments = append(segments, seg)
		}
	}

	for k, vs := range query {
		if utf8.RuneCountInString(k) > MaxQueryRunes {
			return nil, &Refusal{Rule: RuleLength, Reason: fmt.Sprintf("a query key of the URL is longer than %d characters; search with a sentence, not a paragraph", MaxQueryRunes)}
		}

		// A key with no value (?see+main.go+crash) carries words as a
		// value does.
		values = append(values, strings.Fields(k)...)

		for _, v := range vs {
			if utf8.RuneCountInString(v) > MaxQueryRunes {
				return nil, &Refusal{Rule: RuleLength, Reason: fmt.Sprintf("a query value of the URL is longer than %d characters; search with a sentence, not a paragraph", MaxQueryRunes)}
			}

			values = append(values, strings.Fields(v)...)
		}
	}

	for _, seg := range segments {
		if r := shape(seg, false); r != nil {
			return nil, r
		}
	}

	for _, v := range values {
		if r := shape(v, true); r != nil {
			return nil, r
		}
	}

	t, err := g.termsFor(ctx)
	if err != nil {
		return nil, indexRefusal(err)
	}

	for _, tok := range slices.Concat(strings.Split(u.Hostname(), "."), segments, values) {
		if r := t.refusal(bare(tok)); r != nil {
			return nil, r
		}
	}

	return u, nil
}

func indexRefusal(err error) *Refusal {
	return &Refusal{Rule: RuleIndex, Reason: "the blueprint's checkouts could not be read for the guard: " + err.Error()}
}

func (g *Guard) termsFor(ctx context.Context) (compiled, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.loaded {
		return g.terms, nil
	}

	var t Terms

	if g.load != nil {
		var err error
		if t, err = g.load(ctx); err != nil {
			return compiled{}, err
		}
	}

	g.terms, g.loaded = compile(t), true

	return g.terms, nil
}

// fileExtensions are the code, configuration and documentation extensions a
// file-shaped token ends in.
const fileExtensions = `go|py|pyi|ts|tsx|js|jsx|mjs|cjs|rb|rs|java|kt|kts|scala|c|h|cc|cpp|hpp|cs|swift|php|sh|bash|zsh|ps1|sql|proto|ya?ml|json|toml|ini|cfg|conf|env|xml|html?|css|scss|md|txt|lock|mod|sum|tf|tfvars|hcl|gradle|vue|svelte|exs?|erl|hs|lua|pl|dart|zig|clj|groovy|bat|mk|properties|graphql|gql|csv|ipynb`

// structuralPunct is punctuation only code carries: braces, statement ends,
// backticks, assignment, comparison, scope and arrow operators, and shell
// expansions.
const structuralPunct = "[{};`]|:=|=>|->|::|==|!=|&&|\\|\\||\\(\\)|\\$[({A-Za-z]"

// acronymPart is one part of an acronym pair: an acronym or a version.
const acronymPart = `(?:[A-Z0-9]{1,5}|\d+(?:\.\d+)?)`

var (
	// codePunct adds a call's or assignment's operator after a name.
	codePunct = regexp.MustCompile(structuralPunct + "|[A-Za-z0-9_][(=]")
	// segmentPunct leaves that operator out for a URL path segment, where
	// page names carry it: Raft_(algorithm), ms123(v=vs.85).
	segmentPunct = regexp.MustCompile(structuralPunct)
	// acronymPair is the one path-shaped token that passes: CI/CD, TCP/IP,
	// I/O, HTTP/2, HTTP/1.1.
	acronymPair = regexp.MustCompile(`^` + acronymPart + `(?:/` + acronymPart + `)+$`)
	fileName    = regexp.MustCompile(`(?i)^[a-z0-9_.-]*[a-z0-9_-]\.(?:` + fileExtensions + `)$`)
	// jsName is a library named like a file, as JavaScript libraries are:
	// Node.js, Vue.js, D3.js. Its stem is one plain word.
	jsName       = regexp.MustCompile(`(?i)^[a-z0-9]+\.js$`)
	majorVersion = regexp.MustCompile(`^v[0-9]+$`)
)

// shape applies the rules a token breaks by its form alone. A URL path
// segment (all false) skips the path and file rules and is checked against
// segmentPunct.
func shape(tok string, all bool) *Refusal {
	punct := codePunct
	if !all {
		punct = segmentPunct
	}

	if punct.MatchString(tok) {
		return &Refusal{Rule: RuleCode, Token: tok, Reason: "contains code punctuation"}
	}

	if !all {
		return nil
	}

	b := bare(tok)

	if strings.ContainsAny(b, `/\`) && !acronymPair.MatchString(b) {
		return &Refusal{Rule: RulePath, Token: tok, Reason: "contains a path separator"}
	}

	if fileName.MatchString(b) && !jsName.MatchString(b) {
		return &Refusal{Rule: RuleFile, Token: tok, Reason: "looks like a file name"}
	}

	return nil
}

// bare is a token without the quotes, brackets and sentence punctuation
// around it and without a search operator in front (-, +, site: and the
// like).
func bare(tok string) string {
	const around = "\"'“”‘’()[]<>,.!?*"

	t := strings.TrimLeft(strings.Trim(tok, around), "-+")

	if i := strings.IndexByte(t, ':'); i > 0 && isLetters(t[:i]) {
		t = t[i+1:]
	}

	return strings.Trim(t, around)
}

func isLetters(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return false
		}
	}

	return s != ""
}

// compiled is Terms as the guard matches them: each term as its word parts.
type compiled struct {
	identifiers [][]string
	modules     [][]string
	names       [][]string
	card        *regexp.Regexp
}

func compile(t Terms) compiled {
	var c compiled

	for _, id := range t.Identifiers {
		if w := words(id); len(w) >= 2 {
			c.identifiers = append(c.identifiers, w)
		}
	}

	for _, m := range t.Modules {
		for _, seg := range moduleSegments(m) {
			if w := words(seg); len(w) > 0 {
				c.modules = append(c.modules, w)
			}
		}
	}

	for _, n := range t.Names {
		if w := words(n); len(w) > 0 {
			c.names = append(c.names, w)
		}
	}

	var prefixes []string

	for _, p := range t.CardPrefixes {
		if p = strings.TrimSpace(p); p != "" {
			prefixes = append(prefixes, regexp.QuoteMeta(p))
		}
	}

	if len(prefixes) > 0 {
		c.card = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:` + strings.Join(prefixes, "|") + `)-[0-9]+(?:$|[^0-9])`)
	}

	return c
}

// moduleSegments are the parts of a module path that name its owner and
// itself: github.com/acme/payments/v2 gives acme and payments, @acme/web
// gives acme and web, and a bare package name is itself. A host and a major
// version name nothing of the project, and a nested module's further
// segments are ordinary words (api, internal).
func moduleSegments(m string) []string {
	segs := strings.FieldsFunc(strings.TrimSpace(m), func(r rune) bool { return r == '/' })
	if len(segs) > 1 && strings.Contains(segs[0], ".") {
		segs = segs[1:]
	}

	var out []string

	for _, s := range segs {
		s = strings.TrimPrefix(s, "@")
		if s == "" || majorVersion.MatchString(s) {
			continue
		}

		out = append(out, s)
		if len(out) == 2 {
			break
		}
	}

	return out
}

func (c compiled) refusal(tok string) *Refusal {
	if tok == "" {
		return nil
	}

	if c.card != nil && c.card.MatchString(tok) {
		return &Refusal{Rule: RuleCard, Token: tok, Reason: "is a ContextMatrix card id"}
	}

	w := words(tok)

	switch {
	case containsAny(w, c.names):
		return &Refusal{Rule: RuleName, Token: tok, Reason: "names a target project or one of its repositories"}
	case containsAny(w, c.modules):
		return &Refusal{Rule: RuleModule, Token: tok, Reason: "names a module of the blueprint's checkouts"}
	case containsAny(w, c.identifiers):
		return &Refusal{Rule: RuleIdentifier, Token: tok, Reason: "is an identifier declared in the blueprint's checkouts"}
	}

	return nil
}

func containsAny(hay []string, needles [][]string) bool {
	for _, n := range needles {
		if containsRun(hay, n) {
			return true
		}
	}

	return false
}

// containsRun reports whether needle appears in hay as consecutive parts.
func containsRun(hay, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(hay) {
		return false
	}

	for i := 0; i+len(needle) <= len(hay); i++ {
		if slices.Equal(hay[i:i+len(needle)], needle) {
			return true
		}
	}

	return false
}

// words splits s into lowercase word parts at separators, case changes and
// letter-digit boundaries: healthzHandler and healthz_handler both give
// healthz and handler, and HTTPServer gives http and server.
func words(s string) []string {
	var (
		out []string
		cur []rune
	)

	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}

	rs := []rune(s)

	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()

			continue
		}

		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])

			if (unicode.IsLower(prev) && unicode.IsUpper(r)) ||
				(unicode.IsUpper(prev) && unicode.IsUpper(r) && nextLower) ||
				unicode.IsLetter(prev) != unicode.IsLetter(r) {
				flush()
			}
		}

		cur = append(cur, r)
	}

	flush()

	return out
}
