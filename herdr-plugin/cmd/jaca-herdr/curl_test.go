package main

import (
	"strings"
	"testing"
)

// The cases of the app's CurlExportTests.
func curlTxn(method string, headers ...headerPair) netTransaction {
	return netTransaction{Method: method, URL: "https://api.example.com/v1/users?page=2", Host: "api.example.com", RequestHeaders: headers}
}

func TestCurlGetOmitsMethodAndQuotesURL(t *testing.T) {
	got := curlCommand(curlTxn("GET", headerPair{"Accept", "application/json"}), nil)
	want := "curl 'https://api.example.com/v1/users?page=2' \\\n  -H 'Accept: application/json'"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestCurlBodyAndMethod(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		headers    []headerPair
		body       []byte
		has, hasnt []string
	}{
		{"post", "POST", []headerPair{{"Content-Type", "application/json"}}, []byte(`{"name":"ada"}`),
			[]string{"-X POST", "-H 'Content-Type: application/json'", `--data-raw '{"name":"ada"}'`}, nil},
		{"get with a body", "GET", nil, []byte("hi"), []string{"-X GET"}, nil},
		{"empty body", "POST", nil, []byte{}, []string{"-X POST"}, []string{"--data"}},
		{"quotes", "POST", []headerPair{{"X-Note", "it's fine"}}, []byte("don't"),
			[]string{`-H 'X-Note: it'\''s fine'`, `--data-raw 'don'\''t'`}, nil},
		{"pseudo-headers and content length", "GET",
			[]headerPair{{":method", "GET"}, {":authority", "api.example.com"}, {"Content-Length", "42"}, {"Accept", "*/*"}}, nil,
			[]string{"-H 'Accept: */*'"}, []string{":method", ":authority", "Content-Length"}},
		{"unknown encoding", "GET", []headerPair{{"Accept-Encoding", "identity"}}, nil,
			[]string{"-H 'Accept-Encoding: identity'"}, []string{"--compressed"}},
		{"binary body", "POST", nil, []byte{0x00, 0xFF, 0x10}, []string{`--data-binary $'\x00\xff\x10'`}, nil},
		{"method with a dash", "M-SEARCH", nil, nil, []string{"-X 'M-SEARCH'"}, nil},
	}
	for _, c := range cases {
		got := curlCommand(curlTxn(c.method, c.headers...), c.body)
		for _, s := range c.has {
			if !strings.Contains(got, s) {
				t.Errorf("%s: no %q in\n%s", c.name, s, got)
			}
		}
		for _, s := range c.hasnt {
			if strings.Contains(got, s) {
				t.Errorf("%s: %q in\n%s", c.name, s, got)
			}
		}
	}
}

func TestCurlAcceptEncodingBecomesCompressed(t *testing.T) {
	got := curlCommand(curlTxn("GET", headerPair{"Accept-Encoding", "gzip, deflate, br"}), nil)
	if strings.Contains(strings.ToLower(got), "accept-encoding") || !strings.HasSuffix(got, "--compressed") {
		t.Errorf("got\n%s", got)
	}
}

// u, and the detail's Copy cURL button, copy the selected request with its body.
func TestNetCopiesCurl(t *testing.T) {
	v := testNetViewer(t)
	setTermSize(t, 30, 120)
	copied := captureClipboard(t)
	v.bodies["B"] = &netBodies{Request: []byte(`{"user":"ada"}`)}
	want := "curl 'https://auth.example.com/login' \\\n  -X POST \\\n  --data-raw '{\"user\":\"ada\"}'"

	v.selectedID = "B"
	v.handleKey([]byte("u"))
	if got := <-copied; got != want {
		t.Errorf("u copied\n%s\nwant\n%s", got, want)
	}

	v.detail = true
	v.draw()
	if len(v.tabHits) == 0 {
		t.Fatal("no tab row drawn")
	}
	button := v.tabHits[len(v.tabHits)-1]
	v.mouse(mouseEvent{button: 0, x: button.x0, y: v.listTop, press: true})
	if got := <-copied; got != want {
		t.Errorf("Copy cURL copied\n%s\nwant\n%s", got, want)
	}
}
