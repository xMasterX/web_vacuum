package robots

import (
	"encoding/xml"
	"strings"
)

// SitemapEntry is one URL from a sitemap, or a nested sitemap index entry.
type SitemapEntry struct {
	Loc     string
	LastMod string
	IsIndex bool
}

type sitemapXML struct {
	XMLName xml.Name `xml:"urlset"`
	URLs    []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
}

type sitemapIndexXML struct {
	XMLName  xml.Name `xml:"sitemapindex"`
	Sitemaps []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"sitemap"`
}

// ParseSitemap reads either a urlset or a sitemapindex document. Seeding a
// crawl from the sitemap finds pages that are not linked from the home page,
// which is where most "why didn't it download everything" complaints come from.
func ParseSitemap(body []byte) []SitemapEntry {
	var out []SitemapEntry

	var idx sitemapIndexXML
	if err := xml.Unmarshal(body, &idx); err == nil && len(idx.Sitemaps) > 0 {
		for _, s := range idx.Sitemaps {
			loc := strings.TrimSpace(s.Loc)
			if loc != "" {
				out = append(out, SitemapEntry{Loc: loc, LastMod: s.LastMod, IsIndex: true})
			}
		}
		return out
	}

	var set sitemapXML
	if err := xml.Unmarshal(body, &set); err == nil {
		for _, u := range set.URLs {
			loc := strings.TrimSpace(u.Loc)
			if loc != "" {
				out = append(out, SitemapEntry{Loc: loc, LastMod: u.LastMod})
			}
		}
		return out
	}

	// A plain-text sitemap is one URL per line.
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			out = append(out, SitemapEntry{Loc: line})
		}
	}
	return out
}
