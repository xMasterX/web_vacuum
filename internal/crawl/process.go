package crawl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/fetch"
	"github.com/xMasterX/web_vacuum/internal/parse"
	"github.com/xMasterX/web_vacuum/internal/pathmap"
	"github.com/xMasterX/web_vacuum/internal/robots"
	"github.com/xMasterX/web_vacuum/internal/rules"
	"github.com/xMasterX/web_vacuum/internal/state"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// process handles one queued URL from start to finish.
func (e *Engine) process(ctx context.Context, slot int, item *Item) {
	if ctx.Err() != nil {
		return
	}
	if !e.withinFileLimit() || !e.withinByteLimit() {
		return
	}

	// The scope prohibitions are applied a second time, here, immediately before
	// the request goes out.
	//
	// The first application happens when a link is discovered, which is the
	// right place for it but not a sufficient one: settings can change while the
	// job runs, and by the time someone notices that a pattern is filling the
	// queue with junk, tens of thousands of matching URLs are already in it.
	// Filtering only what has yet to be discovered would leave all of them to be
	// downloaded anyway, which is not what anyone means by adding an exclusion.
	//
	// Only the prohibitions are re-applied; see rules.Prohibited for why the
	// permissive half cannot be. Start URLs are exempt because they were queued
	// without consulting the rules in the first place.
	if !item.Seed {
		if d, forbidden := e.rules().Prohibited(item.URL, item.Role, item.Depth); forbidden {
			entry, _ := e.store.Get(item.Key)
			entry.Key, entry.URL, entry.Depth = item.Key, item.URL.String(), item.Depth
			entry.Role = item.Role.String()
			e.finishSkipped(&entry, d.Reason)
			return
		}
	}

	if dead, reason := e.health.dead(hostOf(item.URL)); dead {
		entry, _ := e.store.Get(item.Key)
		entry.Key, entry.URL, entry.Depth = item.Key, item.URL.String(), item.Depth
		entry.Role = item.Role.String()
		e.finishSkipped(&entry, reason)
		return
	}

	e.beginSlot(slot, item)
	defer e.endSlot(slot)

	// The prior record is captured before the entry is marked Active, because
	// the file-replacement policy below needs to know what the last run did.
	prior, hadPrior := e.store.Get(item.Key)
	entry := prior
	entry.Key = item.Key
	entry.URL = item.URL.String()
	entry.Depth = item.Depth
	entry.Role = item.Role.String()
	entry.Referer = item.Referer
	entry.Pass = item.Pass
	entry.Status = state.Active
	e.store.Put(&entry)

	// robots.txt is consulted per host, cached, and skipped entirely when the
	// user has chosen to ignore exclusions.
	if !e.cfg().General.IgnoreRobots {
		r := e.robotsFor(ctx, item.URL)
		pathQuery := item.URL.EscapedPath()
		if item.URL.RawQuery != "" {
			pathQuery += "?" + item.URL.RawQuery
		}
		if !r.Allowed(e.userAgentToken(), pathQuery) {
			e.finishSkipped(&entry, "robots.txt disallows this path")
			return
		}
		if d := r.CrawlDelay(e.userAgentToken()); d > 0 {
			e.client.SetCrawlDelay(item.URL, d)
		}
	}

	// File replacement policy decides whether to skip, refresh conditionally,
	// or re-download outright.
	req := fetch.Request{
		URL:     item.URL,
		Referer: item.Referer,
		Offsite: !e.rules().InScope(item.URL),
		OnPhase: func(phase string) { e.setSlotActivity(slot, phase) },
	}
	if hadPrior && prior.Status == state.Done && prior.Path != "" {
		if abs, err := pathmap.Local(e.cfg().Destination, prior.Path); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				switch e.cfg().General.FileReplacement {
				case config.ReplaceNever:
					if !(e.cfg().General.AlwaysDownloadHTMLCSS && isRefreshable(prior.MediaType, e.rules())) {
						entry.Status = state.Done
						e.store.Put(&entry)
						e.scanExisting(ctx, item, &entry, abs)
						return
					}
				case config.ReplaceNewer:
					req.ETag = prior.ETag
					req.LastModified = prior.LastMod
				}
			}
		}
	}

	resp, err := e.client.Do(ctx, req)
	if err != nil {
		e.noteHostFailure(item.URL, err)
		e.finishFailed(&entry, err)
		return
	}
	e.health.success(hostOf(item.URL))
	defer resp.Body.Close()

	if resp.NotModified {
		// The local copy is current: keep it and still scan it for links, since
		// the frontier may not have those URLs yet on a resumed run.
		entry.Status = state.Done
		entry.HTTPCode = resp.Status
		e.store.Put(&entry)
		if abs, err := pathmap.Local(e.cfg().Destination, entry.Path); err == nil {
			e.scanExisting(ctx, item, &entry, abs)
		}
		e.bus.publish(Event{Kind: EventFetched, URL: entry.URL, Path: entry.Path,
			Status: 304, Depth: item.Depth, Message: "unchanged"})
		return
	}

	mediaType := e.rules().ResolveMediaType(item.URL, resp.MediaType)
	if d := e.rules().CheckResponse(item.URL, mediaType, resp.ContentLength); !d.Allow {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		e.finishSkipped(&entry, d.Reason)
		return
	}

	isHTML := e.rules().IsHTML(mediaType)
	isCSS := e.rules().IsCSS(mediaType)

	// A redirect can land on a different URL than requested; the file is named
	// after where the content actually came from so relative links resolve.
	nameURL := item.URL
	if resp.FinalURL != nil && resp.FinalURL.Host != "" {
		nameURL = urlx.Canonical(resp.FinalURL)
	}
	want := e.mapper.Map(nameURL, pathmap.Hint{
		MediaType: mediaType,
		Filename:  resp.Filename,
		IsHTML:    isHTML,
	})
	rel := e.store.ClaimPath(item.Key, want, pathmap.Disambiguate)
	abs, err := pathmap.Local(e.cfg().Destination, rel)
	if err != nil {
		e.finishFailed(&entry, err)
		return
	}

	e.setSlotResponse(slot, resp.Status, mediaType, resp.ContentLength)

	var content []byte
	var written int64
	if isHTML || isCSS || (e.cfg().Webpage.ScanScripts && len(e.urlPatterns) > 0 && e.rules().IsText(mediaType)) {
		content, err = e.readAll(slot, resp.Body)
		if err == nil {
			written = int64(len(content))
			err = writeFileAtomic(abs, content)
		}
	} else {
		written, err = e.streamToFile(slot, abs, resp.Body)
	}
	// A page that only exists after JavaScript runs is re-fetched through the
	// browser, and what the browser built is what gets saved and scanned.
	var renderedRequests int
	if err == nil && isHTML && e.renderer != nil && e.renderer.wants(nameURL, content) {
		if res, ok := e.renderPage(ctx, slot, nameURL); ok {
			if res.HTML != "" {
				content = []byte(res.HTML)
				if !e.cfg().Render.KeepScripts {
					content = stripScripts(content)
				}
				written = int64(len(content))
				err = writeFileAtomic(abs, content)
			}
			renderedRequests = len(res.Requests)
			e.bus.publish(Event{
				Kind: EventLog, Level: LevelDebug, URL: entry.URL,
				Message: fmt.Sprintf("rendered in %s, %d subresources seen%s",
					res.Elapsed.Round(time.Millisecond), renderedRequests, renderTimeoutNote(res)),
			})
			defer e.queueRendered(item, nameURL, res)
		}
	}

	if err != nil {
		os.Remove(abs + ".part")
		if errors.Is(err, fetch.ErrTooLarge) {
			e.finishSkipped(&entry, "exceeded the maximum file size")
			return
		}
		e.finishFailed(&entry, err)
		return
	}

	// A minimum size can only be enforced once the body is on disk when the
	// server did not send a Content-Length.
	if lim := e.cfg().Limits.MinFileSize.V(); lim > 0 && !isHTML && !isCSS && written < lim {
		os.Remove(abs)
		e.finishSkipped(&entry, fmt.Sprintf("smaller than the %s minimum", e.cfg().Limits.MinFileSize))
		return
	}

	entry.Status = state.Done
	entry.Path = rel
	entry.MediaType = mediaType
	entry.Size = written
	entry.HTTPCode = resp.Status
	entry.ETag = resp.Header.Get("ETag")
	entry.LastMod = resp.Header.Get("Last-Modified")
	entry.Attempts = resp.Attempts
	entry.Err = ""
	entry.ErrKind = ""
	entry.Localized = false
	e.store.Put(&entry)

	// A redirect means the file is named after where the content actually came
	// from, while the entry is keyed on what was asked for. Recording the final
	// URL as well is what lets a link elsewhere on the site — pointing straight
	// at the redirect target — resolve to this file instead of being rewritten
	// back to the live site.
	if finalKey := urlx.Key(nameURL); finalKey != item.Key {
		e.store.AddAlias(finalKey, nameURL.String(), rel)
	}

	e.filesDone.Add(1)
	e.bytesDone.Add(written)
	e.countHost(nameURL)
	e.bus.publish(Event{
		Kind: EventFetched, URL: entry.URL, Path: rel, Status: resp.Status,
		Size: written, Depth: item.Depth, Duration: resp.Elapsed,
		MediaType: mediaType, Attempts: resp.Attempts,
		Rendered: renderedRequests > 0,
	})

	if e.cfg().Limits.MaxBytes > 0 && e.bytesDone.Load() >= e.cfg().Limits.MaxBytes.V() {
		e.Stop(fmt.Sprintf("size limit of %s reached", e.cfg().Limits.MaxBytes))
		return
	}

	if len(content) > 0 {
		e.setSlotActivity(slot, "scanning")
		e.extract(ctx, item, nameURL, content, mediaType, isHTML, isCSS)
	}
}

// scanExisting re-reads a file already on disk so a resumed job can still
// discover the links inside pages it downloaded during an earlier run.
func (e *Engine) scanExisting(ctx context.Context, item *Item, entry *state.Entry, abs string) {
	if entry.Localized {
		// Once rewritten, the file's links point at local paths and are no
		// longer a source of new URLs.
		return
	}
	isHTML := e.rules().IsHTML(entry.MediaType)
	isCSS := e.rules().IsCSS(entry.MediaType)
	if !isHTML && !isCSS {
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil || len(data) > maxTextInMemory {
		return
	}
	e.extract(ctx, item, item.URL, data, entry.MediaType, isHTML, isCSS)
}

// extract finds links in a document and queues the ones in scope.
func (e *Engine) extract(ctx context.Context, item *Item, docURL *url.URL, content []byte, mediaType string, isHTML, isCSS bool) {
	if ctx.Err() != nil {
		return
	}
	var links []parse.Link
	base := docURL

	switch {
	case isHTML:
		doc := parse.HTML(content, e.parseOptions())
		links = doc.Links
		base = parse.ResolveBase(docURL, doc.Base)
		if doc.NoFollowAll && !e.cfg().General.IgnoreNofollow {
			// A robots meta tag asking not to follow links is honoured for
			// hyperlinks, but the page's own assets are still needed.
			links = filterLinks(links, func(l parse.Link) bool {
				return l.Kind != parse.KindHyperlink && l.Kind != parse.KindFrame
			})
		}
	case isCSS:
		links = parse.CSS(content, 0)
	default:
		links = nil
		for _, re := range e.urlPatterns {
			for _, m := range re.FindAllSubmatch(content, -1) {
				v := m[0]
				if len(m) > 1 {
					v = m[1]
				}
				links = append(links, parse.Link{Value: string(v), Kind: parse.KindScript})
			}
		}
	}

	// Off-site pages under the host+1 rule are saved but never expanded, which
	// is what keeps "one external link" from becoming a crawl of the whole web.
	followPages := e.rules().CrawlableForPages(docURL)
	referer := docURL.String()

	for _, l := range links {
		if ctx.Err() != nil {
			return
		}
		if l.Kind == parse.KindBase {
			continue
		}
		if l.NoFollow && !e.cfg().General.IgnoreNofollow {
			continue
		}
		if l.Kind == parse.KindForm && !e.cfg().Webpage.FollowForms {
			continue
		}
		abs, ok := urlx.Resolve(base, l.Value)
		if !ok {
			continue
		}

		role := roleForKind(l.Kind, abs)
		depth := item.Depth
		if role == rules.RolePage {
			if !followPages {
				continue
			}
			depth = item.Depth + 1
		}
		e.enqueue(abs, depth, role, referer, docURL, false)
	}
}

// roleForKind decides whether a link is a page to crawl or a file to fetch.
func roleForKind(k parse.Kind, u *url.URL) rules.Role {
	switch k {
	case parse.KindHyperlink, parse.KindMeta, parse.KindForm:
		return rules.RolePage
	case parse.KindFrame:
		return rules.RolePage
	case parse.KindResource, parse.KindStyle:
		return rules.RoleAsset
	case parse.KindScript:
		// A URL recovered from JavaScript could be either; the extension is the
		// only signal available.
		return rules.GuessRole(u)
	}
	return rules.RoleAsset
}

func filterLinks(in []parse.Link, keep func(parse.Link) bool) []parse.Link {
	out := in[:0:0]
	for _, l := range in {
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

func (e *Engine) parseOptions() parse.Options {
	return parse.Options{
		ScanComments:   e.cfg().Scope.ScanComments,
		ScanSrcset:     e.cfg().Webpage.ScanSrcset,
		ScanInlineCSS:  e.cfg().Webpage.ScanInlineCSS,
		ScanScripts:    e.cfg().Webpage.ScanScripts,
		FollowMeta:     e.cfg().Webpage.FollowMetaRefresh,
		FollowForms:    e.cfg().Webpage.FollowForms,
		DataAttributes: e.cfg().Webpage.DataAttributes,
		ExtraAttrs:     e.cfg().Webpage.ExtraAttributes,
		URLPatterns:    e.urlPatterns,
	}
}

// ---------------------------------------------------------------- outcomes

func (e *Engine) finishSkipped(entry *state.Entry, reason string) {
	entry.Status = state.Skipped
	entry.Err = reason
	e.store.Put(entry)
	e.bus.publish(Event{Kind: EventSkipped, URL: entry.URL, Message: reason, Depth: entry.Depth})
}

func (e *Engine) finishFailed(entry *state.Entry, err error) {
	var fe *fetch.Error
	kind := fetch.KindTransport
	status := 0
	attempts := 0
	if errors.As(err, &fe) {
		kind = fe.Kind
		status = fe.Status
		attempts = fe.Attempts
	}
	if kind == fetch.KindCanceled {
		// A stopped job leaves its work pending so the next run resumes it.
		entry.Status = state.Pending
		e.store.Put(entry)
		return
	}
	entry.Status = state.Failed
	entry.Err = err.Error()
	entry.ErrKind = kind.String()
	entry.HTTPCode = status
	entry.Attempts = attempts
	e.store.Put(entry)
	e.bus.publish(Event{
		Kind: EventFailed, Level: LevelError, URL: entry.URL,
		Status: status, Message: err.Error(), Depth: entry.Depth,
		Attempts: attempts,
	})
}

// ---------------------------------------------------------------- io

// readAll buffers a document, updating the slot's byte counter as it goes.
func (e *Engine) readAll(slot int, r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 64*1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			if buf.Len()+n > maxTextInMemory {
				return nil, fmt.Errorf("document exceeds %s", humanBytes(maxTextInMemory))
			}
			buf.Write(chunk[:n])
			e.addSlotBytes(slot, int64(n))
		}
		if err == io.EOF {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// streamToFile writes a body straight to disk via a .part file, so an
// interrupted download never leaves a truncated file that looks complete.
func (e *Engine) streamToFile(slot int, abs string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	tmp := abs + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	var total int64
	chunk := make([]byte, 256*1024)
	for {
		n, rerr := r.Read(chunk)
		if n > 0 {
			if _, werr := f.Write(chunk[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return total, werr
			}
			total += int64(n)
			e.addSlotBytes(slot, int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return total, rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return total, err
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return total, err
	}
	return total, nil
}

func writeFileAtomic(abs string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp := abs + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, abs)
}

// ---------------------------------------------------------------- limits

func (e *Engine) withinFileLimit() bool {
	lim := e.cfg().Limits.MaxFiles
	if lim <= 0 {
		return true
	}
	if e.filesDone.Load() >= lim {
		e.Stop(fmt.Sprintf("file limit of %d reached", lim))
		return false
	}
	return true
}

func (e *Engine) withinByteLimit() bool {
	lim := e.cfg().Limits.MaxBytes.V()
	if lim <= 0 {
		return true
	}
	return e.bytesDone.Load() < lim
}

// countHost enforces the per-host file cap, which stops one huge subdomain from
// consuming a whole crawl budget.
func (e *Engine) countHost(u *url.URL) {
	lim := e.cfg().Limits.MaxPerHost
	if lim <= 0 {
		return
	}
	h := hostOf(u)
	e.hostCountMu.Lock()
	e.hostCount[h]++
	over := e.hostCount[h] >= lim
	e.hostCountMu.Unlock()
	if over {
		e.logf(LevelWarn, "host %s reached its %d file limit", h, lim)
	}
}

// noteHostFailure feeds a transport failure into the health tracker and says so
// once when a host is written off, rather than repeating it for every URL.
func (e *Engine) noteHostFailure(u *url.URL, err error) {
	var fe *fetch.Error
	if !errors.As(err, &fe) || fe.Kind != fetch.KindTransport {
		return
	}
	reason := "host is unreachable"
	if fe.Err != nil {
		reason = "host is unreachable: " + fe.Err.Error()
	}
	if e.health.failure(hostOf(u), reason) {
		e.logf(LevelWarn, "%s is unreachable — skipping everything else from it", hostOf(u))
	}
}

func isRefreshable(mediaType string, r *rules.Engine) bool {
	return r.IsHTML(mediaType) || r.IsCSS(mediaType)
}

// robotsFor fetches and caches a host's robots.txt.
func (e *Engine) robotsFor(ctx context.Context, u *url.URL) *robots.Robots {
	key := u.Scheme + "://" + u.Host
	return e.robots.Get(key, func() (string, error) {
		body, err := e.fetchBytes(ctx, key+"/robots.txt", 1<<20)
		if err != nil {
			return "", err
		}
		return string(body), nil
	})
}

// userAgentToken is the short name robots.txt groups are matched against.
func (e *Engine) userAgentToken() string {
	ua := config.ResolveUserAgent(e.cfg().Request.UserAgent)
	if ua == "" {
		return "webvacuum"
	}
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		return strings.ToLower(ua[:i])
	}
	return strings.ToLower(ua)
}

// ---------------------------------------------------------------- slots

func (e *Engine) beginSlot(slot int, item *Item) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if slot < 0 || slot >= len(e.slots) {
		return
	}
	e.slots[slot] = SlotState{
		ID: slot + 1, Busy: true, URL: item.URL.String(),
		Host: hostOf(item.URL), Started: time.Now(), Activity: "queued",
	}
}

func (e *Engine) endSlot(slot int) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if slot < 0 || slot >= len(e.slots) {
		return
	}
	e.slots[slot] = SlotState{ID: slot + 1}
}

func (e *Engine) setSlotActivity(slot int, activity string) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if slot >= 0 && slot < len(e.slots) {
		e.slots[slot].Activity = activity
	}
}

// setSlotResponse records what the server said, so a connection row can show a
// status and a size instead of an indefinite "connecting".
func (e *Engine) setSlotResponse(slot int, status int, mediaType string, total int64) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if slot >= 0 && slot < len(e.slots) {
		e.slots[slot].Status = status
		e.slots[slot].MediaType = mediaType
		e.slots[slot].Total = total
		e.slots[slot].Activity = "downloading"
	}
}

func (e *Engine) addSlotBytes(slot int, n int64) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if slot < 0 || slot >= len(e.slots) {
		return
	}
	sl := &e.slots[slot]
	sl.Bytes += n
	if !sl.Started.IsZero() {
		sl.Elapsed = time.Since(sl.Started)
		if secs := sl.Elapsed.Seconds(); secs > 0.05 {
			sl.Speed = float64(sl.Bytes) / secs
		}
	}
}
