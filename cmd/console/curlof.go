package main

import (
	"sort"
	"strings"
)

// curlOf renders a logged call as the curl that would make it again.
//
// Reading a request in the console and reproducing it by hand are two different
// jobs, and the second one is where the mistakes are: a header retyped without
// its key, a body reindented until it is no longer the body that failed. The
// log holds exactly what arrived, so the command is built from that.
func curlOf(base string, d trafficDetail) string {
	var b strings.Builder

	b.WriteString("curl -i -X POST ")
	b.WriteString(shellQuote(endpointOf(base, d)))

	for _, header := range curlHeaders(d.RequestHeaders) {
		b.WriteString(" \\\n  -H ")
		b.WriteString(shellQuote(header.Name + ": " + header.Value))
	}

	if body := strings.TrimSpace(d.RequestBody); body != "" {
		b.WriteString(" \\\n  -d ")
		b.WriteString(shellQuote(body))
	}

	return b.String()
}

// endpointOf is the address this call went to.
//
// The log keeps no URL: a request is recorded against the stand it resolved to,
// which is what the console shows everywhere else. The address is rebuilt from
// the service it reached, which is the same for every call that service serves.
func endpointOf(base string, d trafficDetail) string {
	base = strings.TrimSuffix(base, "/")

	switch d.Service {
	case "merchant":
		return base + ":8081/s/" + d.Sandbox + "/payme/merchant"
	default:
		return base + ":8082/api"
	}
}

// curlHeaders picks the headers worth sending again.
//
// The ones a client sets itself — the length of a body it has not built yet,
// how it would like the answer compressed — are left out, because sending a
// stale Content-Length is how a reproduced request hangs. What stays is the
// credential and the content type, which are what the call actually turned on.
func curlHeaders(headers []accountField) []accountField {
	skip := map[string]bool{
		"content-length":  true,
		"accept-encoding": true,
		"connection":      true,
		"host":            true,
		"user-agent":      true,
	}

	out := make([]accountField, 0, len(headers))
	for _, header := range headers {
		if skip[strings.ToLower(header.Name)] {
			continue
		}
		out = append(out, header)
	}

	// A stable order, so the same call renders the same command every time and
	// two of them can be compared by eye.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	if !hasHeader(out, "content-type") {
		out = append(out, accountField{Name: "Content-Type", Value: "application/json"})
	}

	return out
}

func hasHeader(headers []accountField, name string) bool {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return true
		}
	}

	return false
}

// shellQuote wraps a value so a shell hands it on unchanged, which matters most
// for the body: it is JSON, full of quotes, and a command that mangles it
// reproduces a different request from the one being looked at.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
