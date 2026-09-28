package rss

import "testing"

func TestParse(t *testing.T) {
	rss2 := `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"><channel><title>Blog</title>
<item><title>Post A</title><link>https://b.com/a</link><guid>a1</guid><pubDate>Mon, 28 Sep 2026 10:00:00 +0000</pubDate><description>desc</description><content:encoded><![CDATA[<p>full</p>]]></content:encoded></item></channel></rss>`
	f, err := Parse([]byte(rss2))
	if err != nil || len(f.Items) != 1 || f.Items[0].GUID != "a1" || f.Items[0].Description != "<p>full</p>" || f.Items[0].Published.IsZero() {
		t.Fatalf("rss2: %+v %v", f, err)
	}
	atom := `<feed xmlns="http://www.w3.org/2005/Atom"><title>A</title><entry><title>E</title><id>urn:1</id><link rel="alternate" href="https://x/e"/><updated>2026-09-28T10:00:00Z</updated><summary>s</summary></entry></feed>`
	f, err = Parse([]byte(atom))
	if err != nil || len(f.Items) != 1 || f.Items[0].Link != "https://x/e" || f.Items[0].Feed != "A" {
		t.Fatalf("atom: %+v %v", f, err)
	}
}
