package fetch

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// loadCookieFile reads a Netscape-format cookies.txt, the format every browser
// extension exports. Seeding the jar this way is how a user mirrors a site that
// needs a logged-in session without the tool ever handling a password.
func (c *Client) loadCookieFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cookie jar: %w", err)
	}
	defer f.Close()

	byOrigin := map[string][]*http.Cookie{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		// "#HttpOnly_" is a real prefix, not a comment, in curl's dialect.
		httpOnly := false
		if strings.HasPrefix(text, "#HttpOnly_") {
			httpOnly = true
			text = strings.TrimPrefix(text, "#HttpOnly_")
		} else if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) < 7 {
			fields = strings.Fields(text)
		}
		if len(fields) < 7 {
			continue
		}
		domain := strings.TrimSpace(fields[0])
		includeSub := strings.EqualFold(strings.TrimSpace(fields[1]), "TRUE")
		cpath := strings.TrimSpace(fields[2])
		secure := strings.EqualFold(strings.TrimSpace(fields[3]), "TRUE")
		expires, _ := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
		name := strings.TrimSpace(fields[5])
		value := strings.TrimSpace(strings.Join(fields[6:], "\t"))
		if name == "" || domain == "" {
			continue
		}
		host := strings.TrimPrefix(domain, ".")
		if cpath == "" {
			cpath = "/"
		}
		scheme := "http"
		if secure {
			scheme = "https"
		}
		origin := scheme + "://" + host
		ck := &http.Cookie{
			Name:     name,
			Value:    value,
			Path:     cpath,
			Domain:   domain,
			Secure:   secure,
			HttpOnly: httpOnly,
		}
		if expires > 0 {
			ck.Expires = time.Unix(expires, 0)
			if ck.Expires.Before(time.Now()) {
				continue
			}
		}
		if !includeSub {
			ck.Domain = ""
		}
		byOrigin[origin] = append(byOrigin[origin], ck)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("cookie jar: %w", err)
	}

	for origin, cookies := range byOrigin {
		u, err := url.Parse(origin + "/")
		if err != nil {
			continue
		}
		c.hc.Jar.SetCookies(u, cookies)
	}
	return nil
}
