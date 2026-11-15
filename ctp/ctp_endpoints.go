package ctp

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// AddEndpoint stores a live HTTP(S) page source. Live pages are not pool
// items: each live cue owns its own row (hidden from the pool, the HyperDeck
// clip list and the media worker), so editing one cue's URL never changes
// another cue. It has no file path, duration, thumbnail or waveform.
func AddEndpoint(title, rawURL string) (int, error) {
	title = strings.TrimSpace(title)
	u, err := validateEndpointURL(rawURL)
	if err != nil {
		return 0, err
	}
	u, key := splitScreenKey(u)
	if title == "" {
		title = u.Host
	}
	if key != "" {
		if err := SetScreenKey(u.String(), key); err != nil {
			return 0, err
		}
	}
	var id int
	err = db.QueryRow(`INSERT INTO mediapool (filename, source_kind, endpoint_url, endpoint_title, mimetype,
		size, duration, resolution, thumbnail_pending, waveform, waveform_pending, missing, media_meta)
		VALUES (NULL, 'endpoint', ?, ?, 'application/x-cutepi-endpoint', 0, 0, '', 0, '', 0, 0, '') RETURNING media_id`,
		u.String(), title).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AddLiveCue appends a live-page cue to the end of the sheet (where pool
// adds land too) and selects it, so the inspector opens on it. Returns the
// new cue's position.
// LiveCueFadeInMs is a new live cue's fade-in.
const LiveCueFadeInMs = 1000

func AddLiveCue(title, rawURL string) (int, error) {
	id, err := AddEndpoint(title, rawURL)
	if err != nil {
		return 0, err
	}
	if err := AddEndpointCue(id, "", 0, false); err != nil {
		_, _ = db.Exec(`DELETE FROM mediapool WHERE media_id = ?`, id)
		return 0, err
	}
	var pos int
	if err := db.Get(&pos, `SELECT cuePos FROM cuesheet WHERE media_id = ?`, id); err != nil {
		return 0, err
	}
	// Live pages fade in once loaded (DESIGN §12.14); editable per cue.
	if _, err := db.Exec(`UPDATE cuesheet SET fadeIn = ? WHERE cuePos = ?`, LiveCueFadeInMs, pos); err != nil {
		return pos, err
	}
	if err := SetCue(strconv.Itoa(pos)); err != nil {
		return pos, err
	}
	return pos, nil
}

// SetCueEndpointURL points a live cue at a different page. The cue gets a
// fresh source row: the old one is left orphaned for the live-page cache
// keeper, which drops the old site's cached assets (if no other cue uses
// it) and then the row.
func SetCueEndpointURL(cuePos int, rawURL string) error {
	u, err := validateEndpointURL(rawURL)
	if err != nil {
		return err
	}
	u, key := splitScreenKey(u)
	if key != "" {
		// A pasted key replaces the stored one for that screen.
		if err := SetScreenKey(u.String(), key); err != nil {
			return err
		}
	}
	var cur struct {
		MediaID int    `db:"media_id"`
		URL     string `db:"endpoint_url"`
		Title   string `db:"endpoint_title"`
	}
	err = db.Get(&cur, `SELECT m.media_id, m.endpoint_url, m.endpoint_title FROM cuesheet c
		JOIN mediapool m ON m.media_id = c.media_id
		WHERE c.cuePos = ? AND m.source_kind = 'endpoint'`, cuePos)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("cue %d is not a live page", cuePos)
	}
	if err != nil {
		return err
	}
	if cur.URL == u.String() {
		return nil
	}
	id, err := AddEndpoint(cur.Title, u.String())
	if err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE cuesheet SET media_id = ? WHERE cuePos = ?`, id, cuePos); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// LiveSource is a live-page source row: its id and page URL.
type LiveSource struct {
	MediaID int    `db:"media_id"`
	URL     string `db:"endpoint_url"`
}

// LiveSources lists the live-page sources that cues use.
func LiveSources() ([]LiveSource, error) {
	var out []LiveSource
	err := db.Select(&out, `SELECT media_id, endpoint_url FROM mediapool WHERE source_kind = 'endpoint'
		AND media_id IN (SELECT media_id FROM cuesheet) ORDER BY media_id`)
	return out, err
}

// OrphanEndpoints lists the live-page sources no cue uses any more.
func OrphanEndpoints() ([]LiveSource, error) {
	var out []LiveSource
	err := db.Select(&out, `SELECT media_id, endpoint_url FROM mediapool WHERE source_kind = 'endpoint'
		AND media_id NOT IN (SELECT media_id FROM cuesheet) ORDER BY media_id`)
	return out, err
}

// DeleteOrphanEndpoint removes one unused live-page source row (a no-op if
// a cue has started using it again meanwhile).
func DeleteOrphanEndpoint(mediaID int) error {
	_, err := db.Exec(`DELETE FROM mediapool WHERE media_id = ? AND source_kind = 'endpoint'
		AND media_id NOT IN (SELECT media_id FROM cuesheet)`, mediaID)
	pruneScreenKeys()
	return err
}

// PruneEndpoints removes live-page sources no cue uses any more (their cue
// was deleted, or a show was cleared). They are invisible anyway; this just
// keeps the table tidy. Run at startup.
func PruneEndpoints() error {
	_, err := db.Exec(`DELETE FROM mediapool WHERE source_kind = 'endpoint'
		AND media_id NOT IN (SELECT media_id FROM cuesheet)`)
	pruneScreenKeys()
	return err
}

// ValidateEndpointURL reports whether rawURL is acceptable as a live page:
// an absolute http(s) URL with no embedded credentials.
func ValidateEndpointURL(rawURL string) error {
	_, err := validateEndpointURL(rawURL)
	return err
}

func validateEndpointURL(rawURL string) (*url.URL, error) {
	u, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("endpoint URL must be an absolute http or https URL without embedded credentials")
	}
	return u, nil
}

// Screen keys (DESIGN §12.14, pairing). A TimerPi screen is trusted (stage
// messages, notes, presenter items) when its URL carries the screen's `key`.
// The key is a credential: it is never stored in a cue's URL, so it stays out
// of the sheet, the inspector, exports, logs and WebSocket state. It lives in
// endpoint_secret, keyed by the clean page URL (the screen), and is added back
// only where the renderer opens the page (WithScreenKey).

// screenKeyParam is the query parameter TimerPi reads the screen key from.
const screenKeyParam = "key"

// splitScreenKey returns raw without its screen key and the key ("" if none).
func splitScreenKey(u *url.URL) (clean *url.URL, key string) {
	q := u.Query()
	key = q.Get(screenKeyParam)
	if key == "" && !q.Has(screenKeyParam) {
		return u, ""
	}
	q.Del(screenKeyParam)
	c := *u
	c.RawQuery = q.Encode()
	return &c, key
}

// SetScreenKey stores the key for the page at cleanURL ("" removes it).
func SetScreenKey(cleanURL, key string) error {
	if key == "" {
		_, err := db.Exec(`DELETE FROM endpoint_secret WHERE url = ?`, cleanURL)
		return err
	}
	_, err := db.Exec(`INSERT INTO endpoint_secret (url, secret) VALUES (?, ?)
		ON CONFLICT(url) DO UPDATE SET secret = excluded.secret`, cleanURL, key)
	return err
}

// HasScreenKey reports whether a key is stored for cleanURL.
func HasScreenKey(cleanURL string) bool {
	var n int
	return db.Get(&n, `SELECT COUNT(*) FROM endpoint_secret WHERE url = ?`, cleanURL) == nil && n > 0
}

// WithScreenKey is the URL the renderer opens: cleanURL plus its stored key.
func WithScreenKey(cleanURL string) string {
	var key string
	if db.Get(&key, `SELECT secret FROM endpoint_secret WHERE url = ?`, cleanURL) != nil || key == "" {
		return cleanURL
	}
	u, err := url.Parse(cleanURL)
	if err != nil {
		return cleanURL
	}
	q := u.Query()
	q.Set(screenKeyParam, key)
	u.RawQuery = q.Encode()
	return u.String()
}

// pruneScreenKeys drops keys no live page uses any more.
func pruneScreenKeys() {
	_, _ = db.Exec(`DELETE FROM endpoint_secret WHERE url NOT IN
		(SELECT endpoint_url FROM mediapool WHERE source_kind = 'endpoint')`)
}

// RedactScreenKey hides a screen key in text that may contain a page URL
// (renderer diagnostics), e.g. "...&key=abc" -> "...&key=REDACTED".
func RedactScreenKey(s string) string {
	return screenKeyRe.ReplaceAllString(s, "${1}REDACTED")
}

var screenKeyRe = regexp.MustCompile(`([?&]` + screenKeyParam + `=)[^&\s"']*`)

// WithScreenName gives a TimerPi display page (/d/ or /d/<code>) that names
// no screen the screen name name, so every load of the page (GO, standby,
// cache preload, after a restart) is the same TimerPi screen. TimerPi names a
// display window without ?screen= at random (Screen-XXXX, kept only for that
// window), and each load here is a new WebKit window: without this the unit
// showed up as a new screen every time. Other pages are left as they are.
func WithScreenName(pageURL, name string) string {
	u, err := url.Parse(pageURL)
	if err != nil || name == "" || (u.Path != "/d" && !strings.HasPrefix(u.Path, "/d/")) {
		return pageURL
	}
	q := u.Query()
	if q.Get("screen") != "" {
		return pageURL
	}
	q.Set("screen", name)
	u.RawQuery = q.Encode()
	return u.String()
}
