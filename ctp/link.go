package ctp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// Linked units (DESIGN §6.8c). Paired peers live in link_peer with their
// pairing secret: apart from config.json, never exported, never logged. The
// unit's own link state (mode, stable id, term, show version) is in state
// under link_* keys.

// LinkPeer is one paired unit. Secret is "" for a unit migrated from the old
// linked_peers setting (its URL may then carry a password instead).
type LinkPeer struct {
	ID     string `db:"id"`
	Name   string `db:"name"`
	URL    string `db:"url"`
	Secret string `db:"secret"`
}

// LinkPeers lists the paired units.
func LinkPeers() ([]LinkPeer, error) {
	var out []LinkPeer
	err := db.Select(&out, `SELECT id, name, url, secret FROM link_peer ORDER BY id`)
	return out, err
}

// SaveLinkPeer adds or updates a paired unit.
func SaveLinkPeer(p LinkPeer) error {
	_, err := db.Exec(`INSERT INTO link_peer (id, name, url, secret) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, url = excluded.url, secret = excluded.secret`,
		p.ID, p.Name, p.URL, p.Secret)
	return err
}

// DeleteLinkPeer forgets a paired unit.
func DeleteLinkPeer(id string) error {
	_, err := db.Exec(`DELETE FROM link_peer WHERE id = ?`, id)
	return err
}

// LinkState reads one link_* state value ("" when unset).
func LinkState(key string) string {
	var v string
	_ = db.Get(&v, `SELECT value FROM state WHERE key = ?`, "link_"+key)
	return v
}

// SetLinkState writes link_* state values, given as key, value pairs, in one
// transaction.
func SetLinkState(kv ...string) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := 0; i+1 < len(kv); i += 2 {
		if _, err := tx.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, "link_"+kv[i], kv[i+1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// The replicated show of an active/active pair: cues (every setting, number
// and order), groups, the selection, show mode and GO-advance. Media rows are
// per unit, so a cue names its media by file (or live-page URL), and the
// fire results stay per unit.
var (
	replicatedState = []string{stateKeySelectedCue, stateKeySelectedSet, stateKeyShowMode, stateKeyGoAdvance}
	perUnitCueCols  = []string{"media_id", "last_result", "last_played_at"}
)

// ShowSnapshot is the whole replicated show.
type ShowSnapshot struct {
	Cues   []map[string]any  `json:"cues"`
	Groups []map[string]any  `json:"groups"`
	State  map[string]string `json:"state"`
}

// ExportShowSnapshot returns the replicated show as JSON. The encoding is
// deterministic (rows by id, keys sorted), so equal shows give equal bytes.
func ExportShowSnapshot() ([]byte, error) {
	s := ShowSnapshot{State: map[string]string{}}
	var err error
	if s.Cues, err = mapRows(`SELECT c.*, COALESCE(m.filename, '') AS link_file, m.source_kind AS link_kind,
		m.endpoint_url AS link_url, m.endpoint_title AS link_title, COALESCE(m.mimetype, '') AS link_mime
		FROM cuesheet c JOIN mediapool m ON m.media_id = c.media_id ORDER BY c.cue_id`); err != nil {
		return nil, err
	}
	for _, c := range s.Cues {
		for _, k := range perUnitCueCols {
			delete(c, k)
		}
	}
	if s.Groups, err = mapRows(`SELECT * FROM cue_group ORDER BY group_id`); err != nil {
		return nil, err
	}
	for _, k := range replicatedState {
		var v string
		if err := db.Get(&v, `SELECT value FROM state WHERE key = ?`, k); err == nil {
			s.State[k] = v
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return json.Marshal(s)
}

func mapRows(q string) ([]map[string]any, error) {
	rows, err := db.Queryx(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		m := map[string]any{}
		if err := rows.MapScan(m); err != nil {
			return nil, err
		}
		for k, v := range m {
			if b, ok := v.([]byte); ok {
				m[k] = string(b)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ApplyShowSnapshot replaces this unit's show with snap (one transaction).
// Cues keep their ids and positions; each is pointed at this unit's media
// row for its file, and a file this unit lacks gets a missing pool entry
// (the media diff lists it; a media sync fills it in).
func ApplyShowSnapshot(snap []byte) error {
	var s ShowSnapshot
	if err := json.Unmarshal(snap, &s); err != nil {
		return fmt.Errorf("show snapshot: %w", err)
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cueCols, err := tableColumns(tx, "cuesheet")
	if err != nil {
		return err
	}
	groupCols, err := tableColumns(tx, "cue_group")
	if err != nil {
		return err
	}
	// Kept per unit: each cue's fire result, and its live-page row while the
	// URL is unchanged.
	type local struct {
		CueID   int    `db:"cue_id"`
		MediaID int    `db:"media_id"`
		Result  int    `db:"last_result"`
		Played  int64  `db:"last_played_at"`
		Kind    string `db:"source_kind"`
		URL     string `db:"endpoint_url"`
	}
	var olds []local
	if err := tx.Select(&olds, `SELECT c.cue_id, c.media_id, c.last_result, c.last_played_at, m.source_kind, m.endpoint_url
		FROM cuesheet c JOIN mediapool m ON m.media_id = c.media_id`); err != nil {
		return err
	}
	old := map[int]local{}
	for _, o := range olds {
		old[o.CueID] = o
	}
	for _, q := range []string{`DELETE FROM cuesheet`, `DELETE FROM cue_group`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	for _, g := range s.Groups {
		if err := insertRow(tx, "cue_group", g, groupCols); err != nil {
			return err
		}
	}
	added := false
	for _, c := range s.Cues {
		id := toInt(c["cue_id"])
		file, _ := c["link_file"].(string)
		kind, _ := c["link_kind"].(string)
		u, _ := c["link_url"].(string)
		title, _ := c["link_title"].(string)
		mime, _ := c["link_mime"].(string)
		var mediaID int
		if kind == "endpoint" {
			if o, ok := old[id]; ok && o.Kind == "endpoint" && o.URL == u {
				mediaID = o.MediaID
			} else if mediaID, err = importEndpointTx(tx, title, u); err != nil {
				return err
			}
		} else {
			err := tx.Get(&mediaID, `SELECT media_id FROM mediapool WHERE source_kind = 'file' AND filename = ?`, file)
			if errors.Is(err, sql.ErrNoRows) {
				err = tx.QueryRow(`INSERT INTO mediapool (filename, mimetype, size, duration, resolution, thumbnail_pending, missing)
					VALUES (?, ?, 0, 0, '', 0, 1) RETURNING media_id`, file, mime).Scan(&mediaID)
				added = true
			}
			if err != nil {
				return err
			}
		}
		row := map[string]any{"media_id": mediaID, "last_result": old[id].Result, "last_played_at": old[id].Played}
		for k, v := range c {
			if !strings.HasPrefix(k, "link_") {
				row[k] = v
			}
		}
		if err := insertRow(tx, "cuesheet", row, cueCols); err != nil {
			return err
		}
	}
	for _, k := range replicatedState {
		v, ok := s.State[k]
		if !ok {
			_, err = tx.Exec(`DELETE FROM state WHERE key = ?`, k)
		} else {
			_, err = tx.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v)
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	if added {
		bumpMediaVersion()
	}
	return nil
}

func tableColumns(tx *sqlx.Tx, table string) (map[string]bool, error) {
	var names []string
	if err := tx.Select(&names, `SELECT name FROM pragma_table_info(?)`, table); err != nil {
		return nil, err
	}
	cols := map[string]bool{}
	for _, n := range names {
		cols[n] = true
	}
	return cols, nil
}

// insertRow inserts the columns of row that table has (a peer on another
// build may send columns this one lacks).
func insertRow(tx *sqlx.Tx, table string, row map[string]any, cols map[string]bool) error {
	var names, marks []string
	var args []any
	for k, v := range row {
		if cols[k] {
			names = append(names, `"`+k+`"`)
			marks = append(marks, "?")
			args = append(args, v)
		}
	}
	_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(names, ", "), strings.Join(marks, ", ")), args...)
	return err
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}
