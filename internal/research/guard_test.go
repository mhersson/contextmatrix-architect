package research

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testTerms() Terms {
	return Terms{
		// postgres and Server are single words: declaring them keeps no
		// query from naming them.
		Identifiers:  []string{"healthzHandler", "max_run_cost_usd", "postgres", "Server"},
		Modules:      []string{"github.com/acme/payments/v2"},
		Names:        []string{"alpha", "payments-api"},
		CardPrefixes: []string{"ALPHA"},
	}
}

func testGuard() *Guard {
	return NewGuard(func(context.Context) (Terms, error) { return testTerms(), nil })
}

// refusedRule is the rule err's refusal names: "" for nil, and a
// "not a refusal" line for any other error.
func refusedRule(err error) string {
	if err == nil {
		return ""
	}

	var r *Refusal
	if !errors.As(err, &r) {
		return "not a refusal: " + err.Error()
	}

	return r.Rule
}

func TestGuard_Query(t *testing.T) {
	g := testGuard()

	tests := []struct{ query, rule string }{
		// Plain words and technology names pass.
		{"postgres operator backups", ""},
		{"CloudNativePG versus Zalando postgres operator", ""},
		{"PostgreSQL logical replication", ""},
		{"Node.js stream backpressure", ""},
		{"D3.js force layout", ""},
		{"p5.js sketch setup", ""},
		{"CI/CD pipeline caching", ""},
		{"HTTP/2 server push", ""},
		{"HTTP/1.1 keep-alive", ""},
		{"C++ coroutines and C# records", ""},
		{"site:kubernetes.io kustomize overlays", ""},
		{`"exact phrase" kustomize -helm`, ""},
		{"(CloudNativePG) backups?", ""},
		{"go 1.22 range over func", ""},
		{"server configuration reload", ""},
		// Code-shaped tokens are refused.
		{"internal/server timeout", RulePath},
		{"internal/x layout", RulePath},
		{"https://github.com/acme/x", RulePath},
		{"main.go panics on start", RuleFile},
		{"next.config.js rewrites", RuleFile},
		{"package.json exports field", RuleFile},
		{"foo() returns nil", RuleCode},
		{"x := 1", RuleCode},
		{"map{} literal", RuleCode},
		{"retries=3 default", RuleCode},
		// The blueprint's own terms are refused.
		{"healthzHandler timeout", RuleIdentifier},
		{"HealthzHandler", RuleIdentifier},
		{"healthz_handler_test flakes", RuleIdentifier},
		{"max-run-cost-usd", RuleIdentifier},
		{"acme kubernetes", RuleModule},
		{"payments-api deploy", RuleName},
		{"Alpha rollout", RuleName},
		{"ALPHA-012 status", RuleCard},
		{strings.Repeat("word ", 40), RuleLength},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			err := g.Query(context.Background(), tt.query)
			assert.Equal(t, tt.rule, refusedRule(err), "%v", err)
		})
	}
}

func TestGuard_ARefusalNamesTheRuleAndTheToken(t *testing.T) {
	err := testGuard().Query(context.Background(), "why does healthzHandler time out")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rule identifier")
	assert.Contains(t, err.Error(), `"healthzHandler"`)
}

func TestGuard_ReadsTheTermsOnceAndOnlyWhenNeeded(t *testing.T) {
	loads := 0
	g := NewGuard(func(context.Context) (Terms, error) {
		loads++

		return testTerms(), nil
	})
	ctx := context.Background()

	assert.Equal(t, RuleFile, refusedRule(g.Query(ctx, "main.go panics")))
	assert.Equal(t, 0, loads, "a shape rule refuses before the terms are read")

	require.NoError(t, g.Query(ctx, "postgres operator backups"))
	require.NoError(t, g.Query(ctx, "kustomize overlays"))
	assert.Equal(t, 1, loads)
}

func TestGuard_FailsClosedWhileTheTermsCannotBeRead(t *testing.T) {
	fail, loads := true, 0
	g := NewGuard(func(context.Context) (Terms, error) {
		loads++

		if fail {
			return Terms{}, errors.New("checkout unreachable")
		}

		return testTerms(), nil
	})
	ctx := context.Background()

	err := g.Query(ctx, "postgres operator backups")
	assert.Equal(t, RuleIndex, refusedRule(err))
	require.ErrorContains(t, err, "checkout unreachable")

	fail = false

	require.NoError(t, g.Query(ctx, "postgres operator backups"), "a failed read is retried")
	assert.Equal(t, 2, loads)
}

func TestGuard_URL(t *testing.T) {
	g := testGuard()
	ctx := context.Background()

	tests := []struct{ url, rule string }{
		{"https://cloudnative-pg.io/documentation/current/backup/", ""},
		{"https://github.com/cloudnative-pg/cloudnative-pg/blob/main/docs/src/backup.md", ""},
		{"https://pkg.go.dev/net/http?tab=doc", ""},
		{"https://docs.python.org/3/", ""},
		// A host label leaks like a path segment: the blueprint's own terms
		// are refused there.
		{"https://healthz-handler.attacker.example/", RuleIdentifier},
		{"https://payments-api.attacker.example/x", RuleName},
		{"https://acme.attacker.example/", RuleModule},
		{"https://alpha-12.attacker.example/", RuleCard},
		// A page name in a path may carry a name's ( or =; other code
		// punctuation is still refused there.
		{"https://en.wikipedia.org/wiki/Raft_(algorithm)", ""},
		{"https://en.wikipedia.org/wiki/Raft_%28algorithm%29", ""},
		{"https://en.wikipedia.org/wiki/Go_(programming_language)", ""},
		{"https://learn.microsoft.com/en-us/previous-versions/ms123(v=vs.85)", ""},
		{"https://example.com/docs/{x}", RuleCode},
		{"https://example.com/a;b", RuleCode},
		{"https://example.com/foo()", RuleCode},
		// A path segment, a query key or a query value obeys the query length
		// cap; a long URL of short parts passes.
		{"https://html.duckduckgo.com/html/?q=" + strings.Repeat("word+", 40), RuleLength},
		{"https://example.com/?" + strings.Repeat("word+", 40), RuleLength},
		{"https://example.com/docs/" + strings.Repeat("word%20", 33), RuleLength},
		{"https://example.com/" + strings.Repeat("section/", 60) + "?page=2&lang=en", ""},
		{"file:///etc/passwd", RuleScheme},
		{"ftp://example.com/x", RuleScheme},
		{"https://user:secret@example.com/", RuleCredentials},
		{"https://example.com/search?q=healthzHandler", RuleIdentifier},
		{"https://git.example.com/team/payments-api/README", RuleName},
		{"https://example.com/?q=internal/x", RulePath},
		{"https://example.com/?see+main.go+crash", RuleFile},
		{"https://example.com/a%7Bb", RuleCode},
		{"https://example.com/issues/ALPHA-012", RuleCard},
		{"https://example.com/" + strings.Repeat("x", 3000), RuleLength},
	}

	for _, tt := range tests {
		t.Run(tt.url[:min(len(tt.url), 60)], func(t *testing.T) {
			u, err := g.URL(ctx, tt.url)
			assert.Equal(t, tt.rule, refusedRule(err), "%v", err)

			if tt.rule == "" {
				require.NotNil(t, u)
			}
		})
	}

	// A URL that is not absolute, or whose query does not parse, is an
	// error the model fixes, not a refusal.
	for _, raw := range []string{"example.com/x", "https://example.com/?q=%zz"} {
		_, err := g.URL(ctx, raw)
		require.Error(t, err, raw)
		assert.Contains(t, refusedRule(err), "not a refusal", raw)
	}
}

func TestWords(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"healthzHandler", []string{"healthz", "handler"}},
		{"max_run_cost_usd", []string{"max", "run", "cost", "usd"}},
		{"HTTPServer", []string{"http", "server"}},
		{"PostgreSQL", []string{"postgre", "sql"}},
		{"CloudNativePG", []string{"cloud", "native", "pg"}},
		{"oauth2", []string{"oauth", "2"}},
		{"cert-manager", []string{"cert", "manager"}},
		{"C++", []string{"c"}},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, words(tt.in), tt.in)
	}
}

func TestModuleSegments(t *testing.T) {
	tests := []struct {
		module string
		want   []string
	}{
		{"github.com/acme/payments/v2", []string{"acme", "payments"}},
		{"github.com/acme", []string{"acme"}},
		{"@acme/web", []string{"acme", "web"}},
		{"leftpad", []string{"leftpad"}},
		{"acme.io", []string{"acme.io"}},
		{"example.com/acme/tool/v3/internal/api", []string{"acme", "tool"}},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, moduleSegments(tt.module), tt.module)
	}
}

func TestGuard_ModuleTermsRefuseTheirOwnerAndName(t *testing.T) {
	tests := []struct {
		module, query, rule string
	}{
		{"@acme/web", "acme rollout", RuleModule},
		{"@acme/web", "web rollout", RuleModule},
		{"leftpad", "leftpad rollout", RuleModule},
	}

	for _, tt := range tests {
		g := NewGuard(func(context.Context) (Terms, error) {
			return Terms{Modules: []string{tt.module}}, nil
		})

		assert.Equal(t, tt.rule, refusedRule(g.Query(context.Background(), tt.query)), tt.query)
	}
}
