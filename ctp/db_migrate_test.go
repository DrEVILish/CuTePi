package ctp

import (
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

// migrateLegacyCuesheetDefault must re-brand a legacy volume default of 1.0
// (old linear gain) to 0 (dB) by rebuilding the table, preserving every row
// and the mediapool FK. Fresh DBs (default already 0) are untouched.
func TestMigrateLegacyCuesheetDefaultRebuildsStaleVolume(t *testing.T) {
	d := sqlx.MustConnect("sqlite3", ":memory:")
	// Legacy shape: volume REAL DEFAULT 1.0 (old linear-gain default).
	mustExec(t, d, `CREATE TABLE mediapool (
		media_id INTEGER PRIMARY KEY NOT NULL,
		filename TEXT UNIQUE NOT NULL,
		metadata_json TEXT
	)`)
	mustExec(t, d, `CREATE TABLE cuesheet (
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
		hold INTEGER NOT NULL DEFAULT 1,
		loop INTEGER NOT NULL DEFAULT 0,
		loop_count INTEGER NOT NULL DEFAULT 0,
		color TEXT NOT NULL DEFAULT '',
		parent INTEGER NOT NULL DEFAULT 0,
		fadeOut INTEGER NOT NULL DEFAULT 0,
		fadeAction TEXT NOT NULL DEFAULT 'peers',
		autoContinue INTEGER NOT NULL DEFAULT 0,
		volume REAL NOT NULL DEFAULT 1.0,
		FOREIGN KEY (media_id) REFERENCES mediapool(media_id)
			ON UPDATE CASCADE ON DELETE CASCADE
	)`)
	mustExec(t, d, `INSERT INTO mediapool (media_id, filename) VALUES (1, 'legacy.mp4')`)
	mustExec(t, d, `INSERT INTO cuesheet (cuePos, cueNum, media_id, title, volume, loop, loop_count)
		SELECT 1, '1', 1, 'legacy.mp4', -6, 1, 5`)

	if err := migrateLegacyCuesheetDefault(d); err != nil {
		t.Fatalf("migrateLegacyCuesheetDefault: %v", err)
	}

	var dflt string
	if err := d.Get(&dflt, `SELECT dflt_value FROM pragma_table_info('cuesheet') WHERE name='volume'`); err != nil {
		t.Fatalf("pq: %v", err)
	}
	if dflt != "0" {
		t.Fatalf("volume default = %q, want 0", dflt)
	}

	var vol float64
	if err := d.Get(&vol, `SELECT volume FROM cuesheet WHERE title='legacy.mp4'`); err != nil {
		t.Fatalf("reading migrated row: %v", err)
	}
	if want := -6.0; vol != want {
		t.Fatalf("volume = %v, want %v (data preserved through rebuild)", vol, want)
	}

	var loop, loopCount int
	if err := d.Get(&loop, `SELECT loop FROM cuesheet WHERE title='legacy.mp4'`); err != nil {
		t.Fatalf("reading migrated loop: %v", err)
	}
	if err := d.Get(&loopCount, `SELECT loop_count FROM cuesheet WHERE title='legacy.mp4'`); err != nil {
		t.Fatalf("reading migrated loop_count: %v", err)
	}
	if loop != 1 || loopCount != 5 {
		t.Fatalf("loop/loop_count = %d/%d, want 1/5 (the rebuild must copy both)", loop, loopCount)
	}

	// A second run must be a no-op (default already 0).
	if err := migrateLegacyCuesheetDefault(d); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func mustExec(t *testing.T, d *sqlx.DB, q string) {
	t.Helper()
	if _, err := d.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
