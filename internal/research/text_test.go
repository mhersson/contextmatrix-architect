package research

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

const docHTML = `<html><head><title> Backups | CloudNativePG </title><style>p{}</style></head>
<body><nav><a href="/">Home</a></nav>
<main><h1>Backups</h1><p>Use   <b>barman</b> &amp; object storage.</p>
<ul><li>Daily</li><li>Weekly</li></ul>
<pre>kubectl cnpg backup
  --cluster x</pre>
<div hidden>hidden nav</div><script>var x = 1</script></main>
<footer>Copyright</footer></body></html>`

func TestReadable_KeepsTheContentAndDropsTheFurniture(t *testing.T) {
	title, text := readable([]byte(docHTML))

	assert.Equal(t, "Backups | CloudNativePG", title)
	assert.Equal(t, "# Backups\n\nUse barman & object storage.\n\n- Daily\n- Weekly\n\nkubectl cnpg backup\n  --cluster x", text)
}

func TestReadable_FallsBackToTheTextWhenTheParserRefuses(t *testing.T) {
	body := `<html><head><script>secret</script></head><body>` +
		strings.Repeat("<div>", 600) + "<p>Body sentence.</p>" + strings.Repeat("</div>", 600) + `</body></html>`

	_, perr := html.Parse(strings.NewReader(body))
	require.Error(t, perr, "the markup must be one the parser refuses")

	title, text := readable([]byte(body))

	assert.Empty(t, title)
	assert.Contains(t, text, "Body sentence.")
	assert.NotContains(t, text, "secret")
	assert.NotContains(t, text, "<div")
}

func TestReadable_FallsBackToTheBody(t *testing.T) {
	_, text := readable([]byte(`<html><body><header>Site</header><p>One</p><div aria-hidden="true">x</div><p>Two</p><footer>f</footer></body></html>`))

	assert.Equal(t, "Site\n\nOne\n\nTwo", text)
}
