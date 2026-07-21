package main

import (
	"strings"
	"testing"
)

func TestRewriteNext_AbsoluteUpstream(t *testing.T) {
	in := []byte(`{"next":"https://api.firecrawl.dev/v2/crawl/abc/next?cursor=2","data":[]}`)
	out, changed := rewriteNext(in, "http://api-key-rotator:8788", "api.firecrawl.dev")
	if !changed {
		t.Fatal("expected changed=true")
	}
	// Note: Go's json.Marshal sorts map keys alphabetically, so "data" comes before "next".
	want := `{"data":[],"next":"http://api-key-rotator:8788/v2/crawl/abc/next?cursor=2"}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

func TestRewriteNext_RelativeLeftAlone(t *testing.T) {
	in := []byte(`{"next":"/v2/crawl/abc/next","data":[]}`)
	out, changed := rewriteNext(in, "http://api-key-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for relative next")
	}
	if string(out) != string(in) {
		t.Fatalf("relative next must be untouched, got %s", out)
	}
}

func TestRewriteNext_ForeignHostLeftAlone(t *testing.T) {
	in := []byte(`{"next":"https://example.com/foo","data":[]}`)
	out, changed := rewriteNext(in, "http://api-key-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for foreign host")
	}
	if string(out) != string(in) {
		t.Fatalf("foreign host next must be untouched, got %s", out)
	}
}

func TestRewriteNext_NonURLValueLeftAlone(t *testing.T) {
	in := []byte(`{"next":null,"data":[]}`)
	_, changed := rewriteNext(in, "http://api-key-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for null next")
	}
}

func TestRewriteNext_HostInContentNotRewritten(t *testing.T) {
	// "url" field and scraped markdown mentioning the host must NOT change.
	in := []byte(`{"url":"https://api.firecrawl.dev/page","markdown":"see api.firecrawl.dev for docs"}`)
	out, changed := rewriteNext(in, "http://api-key-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false: host in non-next fields must not be rewritten")
	}
	if string(out) != string(in) {
		t.Fatalf("content corrupted: %s", out)
	}
}

func TestPaginationGuard_NonTerminalNoNext(t *testing.T) {
	// in-progress, more data, no next -> warn
	body := []byte(`{"status":"scraping","completed":3,"total":10,"data":[]}`)
	if !paginationGuard(body) {
		t.Fatal("expected guard=true (warn) for non-terminal no-next")
	}
}

func TestPaginationGuard_TerminalNoNext(t *testing.T) {
	// completed crawl, no next -> normal end, no warn
	body := []byte(`{"status":"completed","completed":10,"total":10,"data":[]}`)
	if paginationGuard(body) {
		t.Fatal("expected guard=false for terminal page")
	}
}

func TestPaginationGuard_HasNext(t *testing.T) {
	body := []byte(`{"status":"scraping","completed":3,"total":10,"next":"https://api.firecrawl.dev/x","data":[]}`)
	if paginationGuard(body) {
		t.Fatal("expected guard=false when next present")
	}
}

func TestRewriteNext_NestedInArray(t *testing.T) {
	in := []byte(`{"items":[{"next":"https://api.firecrawl.dev/x/y"}]}`)
	out, changed := rewriteNext(in, "http://rotator.test", "api.firecrawl.dev")
	if !changed {
		t.Fatal("expected changed=true for nested next in array")
	}
	if !strings.Contains(string(out), "http://rotator.test/x/y") {
		t.Fatalf("expected output to contain proxy URL, got %s", out)
	}
}

func TestRewriteNext_NextUrlKeyNotRewritten(t *testing.T) {
	in := []byte(`{"nextUrl":"https://api.firecrawl.dev/x"}`)
	out, changed := rewriteNext(in, "http://rotator.test", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for nextUrl key")
	}
	if string(out) != string(in) {
		t.Fatalf("expected output unchanged, got %s", out)
	}
}
