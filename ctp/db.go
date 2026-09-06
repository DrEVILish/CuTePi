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
	dsn := dbLocation
	if strings.Contains(dsn, "?") {
		dsn += "&_foreign_keys=on"
	} else {
		dsn += "?_foreign_keys=on"
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
			filename TEXT UNIQUE NOT NULL,
			mimetype TEXT,
			size INTEGER,
			duration REAL,
			resolution TEXT,
			codec TEXT,
			media_title TEXT,
			thumbnail_pending BOOLEAN NOT NULL DEFAULT 1,
			waveform TEXT NOT NULL DEFAULT '',
			waveform_pending BOOLEAN NOT NULL DEFAULT 0,
			missing BOOLEAN NOT NULL DEFAULT 0,
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
	}
	for _, nc := range mpNewCols {
		_, err = db.Exec(fmt.Sprintf(`ALTER TABLE mediapool ADD COLUMN %s %s;`, nc.name, nc.ddl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("ctp: adding %s column: %w", nc.name, err)
		}
	}
	// Rows existing before the waveform column existed hold NULL; backfill to
	// the empty string so SELECTs scan cleanly into Go strings.
	if _, err = db.Exec(`UPDATE mediapool SET waveform = '' WHERE waveform IS NULL;`); err != nil {
		return fmt.Errorf("ctp: backfilling waveform column: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS cuesheet (
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
			FOREIGN KEY (media_id)
				REFERENCES mediapool (media_id)
					ON UPDATE CASCADE
					ON DELETE CASCADE
		);
	`)
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
	newCols := []struct{ name, ddl string }{
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
	}
	for _, nc := range newCols {
		_, err = db.Exec(fmt.Sprintf(`ALTER TABLE cuesheet ADD COLUMN %s %s;`, nc.name, nc.ddl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("ctp: adding %s column: %w", nc.name, err)
		}
	}

	// volume changed meaning from linear gain (1.0 = 0dB) to dB (0 = 0dB).
	// Rescale any rows written under the old linear interpretation once; rows
	// at the old default 1.0 become 0 dB.
	_, _ = db.Exec(`UPDATE cuesheet SET volume = ROUND(20*LOG10(volume)*10)/10
		WHERE volume > 0 AND volume != 1.0;`)
	_, _ = db.Exec(`UPDATE cuesheet SET volume = 0 WHERE volume = 1.0;`)

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
			shuffle          BOOLEAN NOT NULL DEFAULT 0,
			loop             BOOLEAN NOT NULL DEFAULT 0,
			fade_ms          INTEGER NOT NULL DEFAULT 0,
			duration_ms      INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return fmt.Errorf("ctp: creating cue_group table: %w", err)
	}
	newGroupCols := []struct{ name, ddl string }{
		{"collapse", "BOOLEAN NOT NULL DEFAULT 0"},
		{"slideshow", "BOOLEAN NOT NULL DEFAULT 0"},
		{"shuffle", "BOOLEAN NOT NULL DEFAULT 0"},
		{"loop", "BOOLEAN NOT NULL DEFAULT 0"},
		{"fade_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"duration_ms", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, nc := range newGroupCols {
		_, err = db.Exec(fmt.Sprintf(`ALTER TABLE cue_group ADD COLUMN %s %s;`, nc.name, nc.ddl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("ctp: adding %s column on cue_group: %w", nc.name, err)
		}
	}

	return nil
}

func CloseDB() {
	if db != nil {
		db.Close()
	}
}

// migrateLegacyCuesheetDefault rebuilds a legacy cuesheet table whose volume
// column default drifted from linear gain (1.0) to the current dB default (0).
// SQLite has no ALTER ... DEFAULT, so the table is recreated with the code's
// current DDL and all rows copied over. It is a no-op when the default is
// already 0 (fresh DBs). The mediapool FK is preserved; rebuilds that would
// orphan nothing are safe, and a failure aborts the migration (rolled back there
// is a transaction for the DDL runs as a single Exec).
func migrateLegacyCuesheetDefault(d *sqlx.DB) error {
	var staleDefault string
	err := d.Get(&staleDefault, `SELECT dflt_value FROM pragma_table_info('cuesheet') WHERE name='volume'`)
	if err != nil || staleDefault == "0" {
		return err
	}
	_, err = d.Exec(`
		PRAGMA foreign_keys = OFF;
		BEGIN;
		CREATE TABLE cuesheet_new (
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
			volume REAL NOT NULL DEFAULT 0,
			FOREIGN KEY (media_id) REFERENCES mediapool (media_id)
				ON UPDATE CASCADE ON DELETE CASCADE
		);
		INSERT INTO cuesheet_new
			(cue_id, cuePos, cueNum, media_id, title, posStart, posEnd,
			 preWait, cueDuration, postWait, hold, loop, color, parent,
			 fadeOut, fadeAction, autoContinue, volume)
		SELECT cue_id, cuePos, cueNum, media_id, title, posStart, posEnd,
			 preWait, cueDuration, postWait, hold, loop, color, parent,
			 fadeOut, fadeAction, autoContinue, volume
		FROM cuesheet;
		DROP TABLE cuesheet;
		ALTER TABLE cuesheet_new RENAME TO cuesheet;
		COMMIT;
		PRAGMA foreign_keys = ON;
	`)
	if err != nil {
		return fmt.Errorf("ctp: rebuilding legacy cuesheet table: %w", err)
	}
	return nil
}
