package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/hibooboo2/gradio/db"
	"golang.org/x/net/publicsuffix"
)

// domainForURL returns the registrable domain (eTLD+1) for the given stream
// URL, e.g. "https://stream.example.co.uk:8443/live" -> "example.co.uk".
//
// The port is stripped, the host is lowercased, and IP addresses are returned
// as-is (they have no registrable domain). When the host is not a valid public
// suffix (e.g. an unknown TLD), it falls back to the last two labels so the
// domain grouping still works for unusual hosts.
func domainForURL(urlStr string) string {
	if urlStr == "" {
		return ""
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	if eTLD1, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return eTLD1
	}
	labels := strings.Split(host, ".")
	if len(labels) >= 2 {
		return strings.Join(labels[len(labels)-2:], ".")
	}
	return host
}

// domainForStation returns the registrable domain for a station's stream URL.
func domainForStation(s db.RadioStation) string {
	return domainForURL(s.URLResolved)
}
