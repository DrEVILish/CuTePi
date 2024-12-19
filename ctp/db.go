package ctp

import (
	"CuTePi/config"

	_ "github.com/mattn/go-sqlite3" // sqlite3 driver:
	"github.com/jmoiron/sqlx"
)

var db *sqlx.DB

func initDB() {
	var err error
	dbLocation := config.DbLocation()
	db, err = sqlx.Open("sqlite3", dbLocation)
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)


	// Check and create tables if they do not exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS mediapool (
			media_id INTEGER PRIMARY KEY NOT NULL,
			filename TEXT UNIQUE NOT NULL,
			mimetype TEXT,
			size INTEGER,
			duration REAL
		);
	`)
	if err != nil {
		panic(err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS cuesheet (
			cue_id INTEGER PRIMARY KEY NOT NULL,
			cuePos INTEGER UNIQUE,
			cueNum TEXT UNIQUE,
			media_id INTEGER NOT NULL,
			title TEXT UNIQUE NOT NULL,
			posStart INTEGER,
			posEnd INTEGER,
			FOREIGN KEY (media_id)
				REFERENCES mediapool (media_id)
					ON UPDATE CASCADE
					ON DELETE CASCADE
		);
	`)
	if err != nil {
		panic(err)
	}
}

func CloseDB() {
	if db != nil {
		db.Close()
	}
}

func init() {
	initDB()
}
