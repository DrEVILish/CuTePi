package ctp

import (
	"fmt"
	"strings"
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

// The legacy rebuild keeps every cue field the old table has, not just the
// original columns: schedules, sheet order, fade curve, results, geometry
// and crops all survive, and the rebuilt table has every current column.
func TestMigrateLegacyCuesheetKeepsNewerFields(t *testing.T) {
	d := sqlx.MustConnect("sqlite3", ":memory:")
	d.SetMaxOpenConns(1)
	mustExec(t, d, `CREATE TABLE mediapool (media_id INTEGER PRIMARY KEY NOT NULL, filename TEXT UNIQUE)`)
	// A current table except for the stale linear volume default.
	legacy := strings.Replace(fmt.Sprintf(cuesheetDDL, "cuesheet"), "volume REAL NOT NULL DEFAULT 0", "volume REAL NOT NULL DEFAULT 1.0", 1)
	if legacy == fmt.Sprintf(cuesheetDDL, "cuesheet") {
		t.Fatal("test setup: volume default not found in cuesheetDDL")
	}
	mustExec(t, d, legacy)
	if err := addCuesheetColumns(d, "cuesheet"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, d, `INSERT INTO mediapool (media_id, filename) VALUES (1, 'a.mp4')`)
	mustExec(t, d, `INSERT INTO cuesheet (cuePos, cueNum, media_id, title, volume,
		schedule_enabled, schedule_days, schedule_time_ms, sheet_index, fade_curve,
		last_result, last_played_at, opacity, geom_x, geom_w, crop_l, crop_b)
		VALUES (1, '1', 1, 'a', -6, 1, 31, 68400000, 7.5, 'smooth', 2, 1791000000000, 40, '10%', '50%', '8px', '3%')`)

	if err := migrateLegacyCuesheetDefault(d); err != nil {
		t.Fatalf("migrateLegacyCuesheetDefault: %v", err)
	}
	var row struct {
		Volume   float64 `db:"volume"`
		SchedOn  int     `db:"schedule_enabled"`
		Days     int     `db:"schedule_days"`
		TimeMs   int     `db:"schedule_time_ms"`
		Index    float64 `db:"sheet_index"`
		Curve    string  `db:"fade_curve"`
		Result   int     `db:"last_result"`
		PlayedAt int64   `db:"last_played_at"`
		Opacity  float64 `db:"opacity"`
		GeomX    string  `db:"geom_x"`
		GeomW    string  `db:"geom_w"`
		CropL    string  `db:"crop_l"`
		CropB    string  `db:"crop_b"`
	}
	if err := d.Get(&row, `SELECT volume, schedule_enabled, schedule_days, schedule_time_ms, sheet_index, fade_curve,
		last_result, last_played_at, opacity, geom_x, geom_w, crop_l, crop_b FROM cuesheet WHERE title = 'a'`); err != nil {
		t.Fatalf("reading the migrated row: %v", err)
	}
	want := row
	want.Volume, want.SchedOn, want.Days, want.TimeMs, want.Index, want.Curve = -6, 1, 31, 68400000, 7.5, "smooth"
	want.Result, want.PlayedAt, want.Opacity, want.GeomX, want.GeomW, want.CropL, want.CropB = 2, 1791000000000, 40, "10%", "50%", "8px", "3%"
	if row != want {
		t.Fatalf("migrated row = %+v\nwant %+v", row, want)
	}
	for _, c := range cuesheetAddedCols {
		var n int
		if err := d.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('cuesheet') WHERE name = ?`, c.name); err != nil || n != 1 {
			t.Errorf("rebuilt table lacks column %s", c.name)
		}
	}
	var fk int
	if err := d.Get(&fk, `PRAGMA foreign_keys`); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d after the rebuild, want 1", fk)
	}
}

// Cues whose opacity the old inspector wrote back as 0 become 100, once:
// a later 0 is the operator's.
func TestRepairZeroOpacityOnce(t *testing.T) {
	d := sqlx.MustConnect("sqlite3", ":memory:")
	mustExec(t, d, `CREATE TABLE state (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL)`)
	mustExec(t, d, `CREATE TABLE cuesheet (cuePos INTEGER, opacity REAL NOT NULL DEFAULT 100)`)
	mustExec(t, d, `INSERT INTO cuesheet (cuePos, opacity) VALUES (1, 0), (2, 40)`)
	if err := repairZeroOpacity(d); err != nil {
		t.Fatal(err)
	}
	var ops []float64
	if err := d.Select(&ops, `SELECT opacity FROM cuesheet ORDER BY cuePos`); err != nil {
		t.Fatal(err)
	}
	if ops[0] != 100 || ops[1] != 40 {
		t.Fatalf("after repair: %v, want [100 40]", ops)
	}
	mustExec(t, d, `UPDATE cuesheet SET opacity = 0 WHERE cuePos = 1`)
	if err := repairZeroOpacity(d); err != nil {
		t.Fatal(err)
	}
	var again float64
	if err := d.Get(&again, `SELECT opacity FROM cuesheet WHERE cuePos = 1`); err != nil || again != 0 {
		t.Fatalf("the repair ran twice: opacity %v (%v)", again, err)
	}
}

// Waveforms left in mediapool by an older binary move to media_waveform, and
// go with their media row.
func TestMoveWaveforms(t *testing.T) {
	d := sqlx.MustConnect("sqlite3", ":memory:?_foreign_keys=on")
	mustExec(t, d, `CREATE TABLE mediapool (media_id INTEGER PRIMARY KEY NOT NULL, waveform TEXT NOT NULL DEFAULT '')`)
	mustExec(t, d, `INSERT INTO mediapool (media_id, waveform) VALUES (1, '[0.5]'), (2, '')`)
	for i := 0; i < 2; i++ { // idempotent
		if err := moveWaveforms(d); err != nil {
			t.Fatal(err)
		}
	}
	var peaks []string
	if err := d.Select(&peaks, `SELECT peaks FROM media_waveform ORDER BY media_id`); err != nil {
		t.Fatal(err)
	}
	var left int
	d.Get(&left, `SELECT COUNT(*) FROM mediapool WHERE waveform <> ''`)
	if len(peaks) != 1 || peaks[0] != "[0.5]" || left != 0 {
		t.Fatalf("moved %v, %d left in mediapool", peaks, left)
	}
	mustExec(t, d, `DELETE FROM mediapool WHERE media_id = 1`)
	var n int
	d.Get(&n, `SELECT COUNT(*) FROM media_waveform`)
	if n != 0 {
		t.Fatalf("waveform outlived its media row")
	}
}
