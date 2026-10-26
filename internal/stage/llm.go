package stage

import (
	"net/http"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// ClientFor builds the harness LLM client for a CM-provisioned endpoint. The
// chat backend maps the same way: "openai" selects the OpenAI dialect, any
// other type the OpenRouter dialect; an empty base URL keeps the dialect's
// default. A non-nil base carries every request, as the transport that
// trusts ca_cert_file; nil keeps the harness's own client.
func ClientFor(ep protocol.LLMEndpoint, base http.RoundTripper) llm.LLM {
	opts := []llm.Option{llm.WithRetry(llm.DefaultRetryPolicy())}

	if base != nil {
		opts = append(opts, llm.WithHTTPClient(&http.Client{Transport: base}))
	}

	if ep.BaseURL != "" {
		opts = append(opts, llm.WithBaseURL(ep.BaseURL))
	}

	if ep.Type == "openai" {
		opts = append(opts, llm.WithDialect(llm.DialectOpenAI))
	}

	return llm.NewClient(ep.APIKey, opts...)
}
