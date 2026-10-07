package ctp

import (
	"fmt"
	"strings"

	"CuTePi/config"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3" // sqlite3 driver:
)

var db *sqlx.DB

// InitDB opens (creating if missing) the sqlite database at the configured
// location and ensures the schema exists. Must be called after
// config.LoadConfig() so it honors any config-file-specified DB path.
func InitDB() error {
	dbLocation := config.DbLocation()
	var err error
	// SQLite does not enforce foreign keys (and therefore ON DELETE CASCADE)
	// unless explicitly turned on per-connection - without this, deleting a
	// media row referenced by a cue leaves an orphaned cuesheet row whose
	// LEFT JOIN produces NULLs that crash the cuesheet scan entirely.
	//
	// WAL + busy_timeout are driver-level DSN pragmas (per-connection, so the
	// DSN is the only place they reliably apply). The app itself serialises
	// on one pooled connection (SetMaxOpenConns below), but external readers
	// do not: e.g. inspecting the show DB with the sqlite3 CLI mid-show, or
	// any second process holding a write transaction, previously caused
	// immediate SQLITE_BUSY errors and truncated/rolled-back reads. WAL lets
	// readers proceed against the pre-write snapshot and busy_timeout makes
	// writers wait (5s) instead of failing instantly.
	dsn := dbLocation
	if strings.Contains(dsn, "?") {
		dsn += "&_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
	} else {
		dsn += "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err = sqlx.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("ctp: opening db at %q: %w", dbLocation, err)
	}
	db.SetMaxOpenConns(1)

	// Check and create tables if they do not exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS mediapool (
			media_id INTEGER PRIMARY KEY NOT NULL,
			filename TEXT UNIQUE,
			source_kind TEXT NOT NULL DEFAULT 'file',
			endpoint_url TEXT NOT NULL DEFAULT '',
			endpoint_title TEXT NOT NULL DEFAULT '',
			mimetype TEXT,
			size INTEGER,
			duration REAL,
			resolution TEXT,
			thumbnail_pending BOOLEAN NOT NULL DEFAULT 1,
			waveform TEXT NOT NULL DEFAULT '',
			waveform_pending BOOLEAN NOT NULL DEFAULT 0,
			missing BOOLEAN NOT NULL DEFAULT 0,
			loudness_gain REAL NOT NULL DEFAULT 0,
			date_added DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return fmt.Errorf("ctp: creating mediapool table: %w", err)
	}

	// Migration: add newer mediapool columns if they don't exist (for DBs
	// created before these columns were added).
	mpNewCols := []struct{ name, ddl string }{
		{"waveform", "TEXT NOT NULL DEFAULT ''"}, // NOT NULL so sqlx can scan into Go string
		{"waveform_pending", "BOOLEAN NOT NULL DEFAULT 0"},
		{"missing", "BOOLEAN NOT NULL DEFAULT 0"},
		{"media_meta", "TEXT NOT NULL DEFAULT ''"}, // JSON MediaInfo for the inspector's Media tab
		{"loudness_gain", "REAL NOT NULL DEFAULT 0"},
		{"source_kind", "TEXT NOT NULL DEFAULT 'file'"},
		{"endpoint_url", "TEXT NOT NULL DEFAULT ''"},
		{"endpoint_title", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, nc := range mpNewCols {
		_, err = db.Exec(fmt.Sprintf(`ALTER TABLE mediapool ADD COLUMN %s %s;`, nc.name, nc.ddl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("ctp: adding %s column: %w", nc.name, err)
		}
	}
	// File-backed entries historically required a filename. Endpoint sources
	// have no file path, so rebuild that table once to make filename nullable.
	// Keep the referenced mediapool table name stable for cuesheet's FK.
	var filenameNotNull int
	if err := db.Get(&filenameNotNull, `SELECT "notnull" FROM pragma_table_info('mediapool') WHERE name = 'filename'`); err != nil {
		return fmt.Errorf("ctp: checking mediapool filename constraint: %w", err)
	}
	if filenameNotNull != 0 {
		if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("ctp: disabling foreign keys for mediapool migration: %w", err)
		}
		tx, err := db.Beginx()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE mediapool_new (
			media_id INTEGER PRIMARY KEY NOT NULL, filename TEXT UNIQUE,
			source_kind TEXT NOT NULL DEFAULT 'file', endpoint_url TEXT NOT NULL DEFAULT '', endpoint_title TEXT NOT NULL DEFAULT '',
			mimetype TEXT, size INTEGER, duration REAL, resolution TEXT,
			thumbnail_pending BOOLEAN NOT NULL DEFAULT 1, waveform TEXT NOT NULL DEFAULT '',
			waveform_pending BOOLEAN NOT NULL DEFAULT 0, missing BOOLEAN NOT NULL DEFAULT 0,
			loudness_gain REAL NOT NULL DEFAULT 0, media_meta TEXT NOT NULL DEFAULT '',
			date_added DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
		if err == nil {
			_, err = tx.Exec(`INSERT INTO mediapool_new (media_id, filename, source_kind, endpoint_url, endpoint_title, mimetype,
				size, duration, resolution, thumbnail_pending, waveform, waveform_pending, missing,
				loudness_gain, media_meta, date_added)
				SELECT media_id, filename, source_kind, endpoint_url, endpoint_title, mimetype, size, duration, resolution,
				thumbnail_pending, waveform, waveform_pending, missing, loudness_gain, media_meta, date_added
				FROM mediapool`)
		}
		if err == nil {
			_, err = tx.Exec(`DROP TABLE mediapool`)
		}
		if err == nil {
			_, err = tx.Exec(`ALTER TABLE mediapool_new RENAME TO mediapool`)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		_, fkErr := db.Exec(`PRAGMA foreign_keys = ON`)
		if err != nil {
			return fmt.Errorf("ctp: making mediapool filename nullable: %w", err)
		}
		if fkErr != nil {
			return fmt.Errorf("ctp: restoring foreign keys after mediapool migration: %w", fkErr)
		}
	}
	// Rows existing before the waveform column existed hold NULL; backfill to
	// the empty string so SELECTs scan cleanly into Go strings.
	if _, err = db.Exec(`UPDATE mediapool SET waveform = '' WHERE waveform IS NULL;`); err != nil {
		return fmt.Errorf("ctp: backfilling waveform column: %w", err)
	}

	// Migration: the codec and media_title columns were write-only (populated
	// by RegisterMedia, read by nothing - no handler, template, export or JS
	// surfaced them). Drop them so SELECT * scans cleanly against the slimmed
	// Media struct.
	for _, dead := range []string{"codec", "media_title"} {
		var present int
		if err := db.Get(&present, fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('mediapool') WHERE name = '%s'`, dead)); err != nil {
			return fmt.Errorf("ctp: checking mediapool column %s: %w", dead, err)
		}
		if present > 0 {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE mediapool DROP COLUMN %s;`, dead)); err != nil {
				return fmt.Errorf("ctp: dropping mediapool column %s: %w", dead, err)
			}
		}
	}

	_, err = db.Exec(fmt.Sprintf(cuesheetDDL, "cuesheet"))
	if err != nil {
		return fmt.Errorf("ctp: creating cuesheet table: %w", err)
	}

	// Migration: the historical autoFollow flag became autoContinue (it now
	// auto-plays the next cue rather than only advancing the selection). Keep
	// its values by renaming the column instead of re-adding with a default.
	var hasAutoFollow int
	if err := db.Get(&hasAutoFollow, `SELECT COUNT(*) FROM pragma_table_info('cuesheet') WHERE name = 'autoFollow'`); err != nil {
		return fmt.Errorf("ctp: reading cuesheet columns: %w", err)
	}
	if hasAutoFollow > 0 {
		if _, err := db.Exec(`ALTER TABLE cuesheet RENAME COLUMN autoFollow TO autoContinue;`); err != nil {
			return fmt.Errorf("ctp: renaming autoFollow to autoContinue: %w", err)
		}
	}

	// Migration: add newer cuesheet columns if they don't exist (for DBs
	// created before these columns were added)
	if err := addCuesheetColumns(db, "cuesheet"); err != nil {
		return err
	}

	// Rationalize a legacy cuesheet table whose volume column was created with
	// the old linear default (dflt_value 1.0). SQLite cannot change a column
	// default without rebuilding the table, so rebuild to the current schema
	// (volume DEFAULT 0) once, preserving all rows and the mediapool FK. This
	// also re-brands any other drifted defaults to the code's DDL. Only runs
	// when the default is actually stale, so fresh DBs are untouched.
	if err := migrateLegacyCuesheetDefault(db); err != nil {
		return err
	}

	// Small server-authoritative key/value store. Selection is persisted here
	// so it survives a reload and is shared across clients (see
	// SelectedCuePos / SetCue).
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS state (
			key   TEXT PRIMARY KEY NOT NULL,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("ctp: creating state table: %w", err)
	}
	if err := repairZeroOpacity(db); err != nil {
		return err
	}

	// Cue groups: visual folders holding cues (cuesheet.parent = group_id).
	// Slideshow settings live on the group so image groups can play shuffled /
	// looped / faded with a duration-per-image. Folder membership is a
	// presentation layer on top of the flat cuePos order.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS cue_group (
			group_id         INTEGER PRIMARY KEY NOT NULL,
			name             TEXT NOT NULL,
			parent_group_id  INTEGER NOT NULL DEFAULT 0,
			collapse         BOOLEAN NOT NULL DEFAULT 0,
			slideshow        BOOLEAN NOT NULL DEFAULT 0,
			awards_mode      BOOLEAN NOT NULL DEFAULT 0,
			shuffle          BOOLEAN NOT NULL DEFAULT 0,
			loop             BOOLEAN NOT NULL DEFAULT 0,
			fade_ms          INTEGER NOT NULL DEFAULT 0,
			duration_ms      INTEGER NOT NULL DEFAULT 0,
			anchor_pos       INTEGER NOT NULL DEFAULT 0,
			sheet_index      REAL NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return fmt.Errorf("ctp: creating cue_group table: %w", err)
	}
	newGroupCols := []struct{ name, ddl string }{
		{"anchor_pos", "INTEGER NOT NULL DEFAULT 0"},
		{"sheet_index", "REAL NOT NULL DEFAULT 0"},
		{"collapse", "BOOLEAN NOT NULL DEFAULT 0"},
		{"slideshow", "BOOLEAN NOT NULL DEFAULT 0"},
		{"awards_mode", "BOOLEAN NOT NULL DEFAULT 0"},
		{"shuffle", "BOOLEAN NOT NULL DEFAULT 0"},
		{"loop", "BOOLEAN NOT NULL DEFAULT 0"},
		{"fade_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"duration_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"cue_num", "TEXT NOT NULL DEFAULT ''"},
		{"color", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, nc := range newGroupCols {
		_, err = db.Exec(fmt.Sprintf(`ALTER TABLE cue_group ADD COLUMN %s %s;`, nc.name, nc.ddl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("ctp: adding %s column on cue_group: %w", nc.name, err)
		}
	}
	// Backfill the visual order once: sheet_index rides cuePos (x1000 so
	// group headers can slot before their blocks).
	if _, err := db.Exec(`UPDATE cuesheet SET sheet_index = cuePos * 1000 WHERE sheet_index = 0;`); err != nil {
		return fmt.Errorf("ctp: backfilling cuesheet sheet_index: %w", err)
	}
	if _, err := db.Exec(`UPDATE cue_group SET sheet_index = COALESCE(
		(SELECT MIN(c.sheet_index) - 500 FROM cuesheet c WHERE c.parent = cue_group.group_id),
		(SELECT (COALESCE(MAX(cuePos), 0) + 1) * 1000 FROM cuesheet))
		WHERE sheet_index = 0;`); err != nil {
		return fmt.Errorf("ctp: backfilling cue_group sheet_index: %w", err)
	}

	return nil
}

func CloseDB() {
	if db != nil {
		db.Close()
	}
}

// cuesheetDDL creates the cuesheet table (%s: its name). Fresh databases
// and the legacy rebuild below use the same definition, then the same
// cuesheetAddedCols, so a rebuilt table has every column a fresh one has.
const cuesheetDDL = `
		CREATE TABLE IF NOT EXISTS %s (
			cue_id INTEGER PRIMARY KEY NOT NULL,
			cuePos INTEGER UNIQUE,
			cueNum TEXT UNIQUE,
			media_id INTEGER NOT NULL,
			title TEXT UNIQUE NOT NULL,
			posStart INTEGER NOT NULL DEFAULT 0,
			posEnd INTEGER NOT NULL DEFAULT 0,
			preWait INTEGER NOT NULL DEFAULT 0,
			cueDuration INTEGER NOT NULL DEFAULT 0,
			postWait INTEGER NOT NULL DEFAULT 0,
			hold INTEGER NOT NULL DEFAULT 0,
			loop INTEGER NOT NULL DEFAULT 0,
			loop_count INTEGER NOT NULL DEFAULT 0,
			color TEXT NOT NULL DEFAULT '',
			parent INTEGER NOT NULL DEFAULT 0,
			fadeOut INTEGER NOT NULL DEFAULT 0,
			fadeAction TEXT NOT NULL DEFAULT 'peers',
			autoContinue INTEGER NOT NULL DEFAULT 0,
			volume REAL NOT NULL DEFAULT 0, -- per-cue master gain in dB; 0 = 0dB
			fadeIn INTEGER NOT NULL DEFAULT 0,
			rate REAL NOT NULL DEFAULT 1,
			balance REAL NOT NULL DEFAULT 0,
			mute INTEGER NOT NULL DEFAULT 0,
	last_result INTEGER NOT NULL DEFAULT 0, -- 0 never, 1 ok, 2 error
	last_played_at INTEGER NOT NULL DEFAULT 0, -- unix ms
		fade_curve TEXT NOT NULL DEFAULT 'linear', -- linear|smooth|log|exp
		fit_mode TEXT NOT NULL DEFAULT 'fit', -- fit|stretch frame fitting
		rotation INTEGER NOT NULL DEFAULT 0, -- 0|90|180|270 clockwise degrees
		flip TEXT NOT NULL DEFAULT 'none', -- none|h|v mirror
		schedule_enabled INTEGER NOT NULL DEFAULT 0, -- boolean: whether scheduling is enabled
	schedule_days INTEGER NOT NULL DEFAULT 0,    -- bitmask: bit0=Mon, bit1=Tue, ..., bit6=Sun
	schedule_time_ms INTEGER NOT NULL DEFAULT 0, -- time of day in milliseconds since 00:00:00
	sheet_index REAL NOT NULL DEFAULT 0, -- visual+playback order (§4)
			FOREIGN KEY (media_id)
				REFERENCES mediapool (media_id)
					ON UPDATE CASCADE
					ON DELETE CASCADE
		);`

// cuesheetAddedCols are the cuesheet columns added after the original
// table: added to older databases on startup, and to a rebuilt table.
var cuesheetAddedCols = []struct{ name, ddl string }{
	{"preWait", "INTEGER NOT NULL DEFAULT 0"},
	{"cueDuration", "INTEGER NOT NULL DEFAULT 0"},
	{"postWait", "INTEGER NOT NULL DEFAULT 0"},
	{"hold", "INTEGER NOT NULL DEFAULT 0"},
	{"loop", "INTEGER NOT NULL DEFAULT 0"},
	{"loop_count", "INTEGER NOT NULL DEFAULT 0"},
	{"color", "TEXT NOT NULL DEFAULT ''"},
	{"parent", "INTEGER NOT NULL DEFAULT 0"},
	{"fadeOut", "INTEGER NOT NULL DEFAULT 0"},
	{"fadeAction", "TEXT NOT NULL DEFAULT 'peers'"},
	{"autoContinue", "INTEGER NOT NULL DEFAULT 0"},
	{"volume", "REAL NOT NULL DEFAULT 0"},
	{"fadeIn", "INTEGER NOT NULL DEFAULT 0"},
	{"rate", "REAL NOT NULL DEFAULT 1"},
	{"balance", "REAL NOT NULL DEFAULT 0"},
	{"mute", "INTEGER NOT NULL DEFAULT 0"},
	{"last_result", "INTEGER NOT NULL DEFAULT 0"},
	{"last_played_at", "INTEGER NOT NULL DEFAULT 0"},
	{"fade_curve", "TEXT NOT NULL DEFAULT 'linear'"},
	{"fit_mode", "TEXT NOT NULL DEFAULT 'fit'"},
	{"rotation", "INTEGER NOT NULL DEFAULT 0"},
	{"flip", "TEXT NOT NULL DEFAULT 'none'"},
	{"schedule_enabled", "INTEGER NOT NULL DEFAULT 0"},
	{"schedule_days", "INTEGER NOT NULL DEFAULT 0"},
	{"schedule_time_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"sheet_index", "REAL NOT NULL DEFAULT 0"},
	{"opacity", "REAL NOT NULL DEFAULT 100"},
	{"geom_x", "TEXT NOT NULL DEFAULT ''"},
	{"geom_y", "TEXT NOT NULL DEFAULT ''"},
	{"geom_w", "TEXT NOT NULL DEFAULT ''"},
	{"geom_h", "TEXT NOT NULL DEFAULT ''"},
	{"crop_l", "TEXT NOT NULL DEFAULT ''"},
	{"crop_r", "TEXT NOT NULL DEFAULT ''"},
	{"crop_t", "TEXT NOT NULL DEFAULT ''"},
	{"crop_b", "TEXT NOT NULL DEFAULT ''"},
	{"stop_others", "INTEGER NOT NULL DEFAULT 1"}, // fire stops the running cues (§6.1.2)
	{"layer", "TEXT NOT NULL DEFAULT 'top'"},      // top|bottom|under when it keeps them
	{"layer_under", "INTEGER NOT NULL DEFAULT 0"}, // under: the cue_id to go beneath
}

// addCuesheetColumns adds every cuesheetAddedCols column that table lacks.
func addCuesheetColumns(d sqlx.Execer, table string) error {
	for _, nc := range cuesheetAddedCols {
		_, err := d.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s;`, table, nc.name, nc.ddl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("ctp: adding %s column: %w", nc.name, err)
		}
	}
	return nil
}

// repairZeroOpacity runs once per database. Until 2026-10-07 cues were
// loaded without their opacity, so the inspector showed 0 and wrote 0 back
// on every save; playback reads 0 as opaque, so nothing looked wrong. Now the
// inspector shows the stored value, so those cues are set to 100 (the
// picture does not change).
func repairZeroOpacity(d *sqlx.DB) error {
	const key = "repair_zero_opacity"
	var done int
	if err := d.Get(&done, `SELECT COUNT(*) FROM state WHERE key = ?`, key); err != nil || done > 0 {
		return err
	}
	if _, err := d.Exec(`UPDATE cuesheet SET opacity = 100 WHERE opacity = 0`); err != nil {
		return fmt.Errorf("ctp: repairing cue opacity: %w", err)
	}
	_, err := d.Exec(`INSERT INTO state (key, value) VALUES (?, '1')`, key)
	return err
}

// migrateLegacyCuesheetDefault rebuilds a legacy cuesheet table whose volume
// column default drifted from linear gain (1.0) to the current dB default (0).
// SQLite has no ALTER ... DEFAULT, so the table is recreated from the
// current definition (cuesheetDDL plus cuesheetAddedCols) and every column
// the old and new tables share is copied over: no cue field is lost
// (schedules, order, geometry, fades, results), whatever the old table's
// age. It runs in one transaction, so a failure leaves the old table as it
// was. A no-op when the default is already 0 (fresh DBs).
func migrateLegacyCuesheetDefault(d *sqlx.DB) error {
	var staleDefault string
	err := d.Get(&staleDefault, `SELECT dflt_value FROM pragma_table_info('cuesheet') WHERE name='volume'`)
	if err != nil || staleDefault == "0" {
		return err
	}
	// Foreign keys off for the swap (DROP TABLE would cascade-delete
	// nothing here, but a rename under enforcement can fail). The pragma is
	// a no-op inside a transaction, so it brackets it.
	if _, err := d.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("ctp: legacy cuesheet rebuild: %w", err)
	}
	defer d.Exec(`PRAGMA foreign_keys = ON`)
	tx, err := d.Beginx()
	if err != nil {
		return fmt.Errorf("ctp: legacy cuesheet rebuild: %w", err)
	}
	defer tx.Rollback() // no-op after Commit
	if _, err := tx.Exec(fmt.Sprintf(cuesheetDDL, "cuesheet_new")); err != nil {
		return fmt.Errorf("ctp: legacy cuesheet rebuild: creating table: %w", err)
	}
	if err := addCuesheetColumns(tx, "cuesheet_new"); err != nil {
		return err
	}
	var cols []string
	if err := tx.Select(&cols, `SELECT o.name FROM pragma_table_info('cuesheet') o
		JOIN pragma_table_info('cuesheet_new') n ON n.name = o.name ORDER BY o.cid`); err != nil {
		return fmt.Errorf("ctp: legacy cuesheet rebuild: reading columns: %w", err)
	}
	list := `"` + strings.Join(cols, `", "`) + `"`
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO cuesheet_new (%s) SELECT %s FROM cuesheet`, list, list),
		`DROP TABLE cuesheet`,
		`ALTER TABLE cuesheet_new RENAME TO cuesheet`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("ctp: legacy cuesheet rebuild: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ctp: legacy cuesheet rebuild: %w", err)
	}
	return nil
}
