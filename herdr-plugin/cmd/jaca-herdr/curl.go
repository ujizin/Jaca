package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// curlCommand is the app's CurlExport.command: the request as a curl command that runs as-is,
// with body as its request body (nil when it has none).
func curlCommand(t netTransaction, body []byte) string {
	parts := []string{"curl " + shellQuote(t.URL)}

	method := strings.ToUpper(t.Method)
	// curl sends GET, or POST once there is a body: the method is spelled out only otherwise.
	if method != "GET" || len(body) > 0 {
		parts = append(parts, "-X "+methodToken(method))
	}

	compressed := false
	for _, h := range t.RequestHeaders {
		name := strings.ToLower(h.Name)
		// HTTP/2 pseudo-headers aren't headers, and curl sets Content-Length from the body.
		if strings.HasPrefix(name, ":") || name == "content-length" {
			continue
		}
		// Without --compressed curl prints the compressed bytes, so the flag takes the header's place.
		if name == "accept-encoding" && isCompressionOffer(h.Value) {
			compressed = true
			continue
		}
		parts = append(parts, "-H "+shellQuote(h.Name+": "+h.Value))
	}

	if len(body) > 0 {
		parts = append(parts, dataArgument(body))
	}
	if compressed {
		parts = append(parts, "--compressed")
	}
	return strings.Join(parts, " \\\n  ")
}

func methodToken(method string) string {
	for _, r := range method {
		if !unicode.IsLetter(r) {
			return shellQuote(method)
		}
	}
	return method
}

func isCompressionOffer(value string) bool {
	lowered := strings.ToLower(value)
	for _, coding := range []string{"gzip", "deflate", "br", "zstd"} {
		if strings.Contains(lowered, coding) {
			return true
		}
	}
	return false
}

// dataArgument is the body as text, or byte by byte in ANSI-C quotes when it isn't UTF-8.
func dataArgument(body []byte) string {
	if utf8.Valid(body) {
		return "--data-raw " + shellQuote(string(body))
	}
	var b strings.Builder
	for _, c := range body {
		fmt.Fprintf(&b, "\\x%02x", c)
	}
	return "--data-binary $'" + b.String() + "'"
}
