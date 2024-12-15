package ctp

import (
	"CuTePi/config"

	_ "github.com/mattn/go-sqlite3" // sqlite3 driver:
	"github.com/jmoiron/sqlx"
)

var db *sqlx.DB

func initDB() {
	var err error
	dbLocation := *config.DbLocation() // Dereference the pointer to get the string value
	db, err = sqlx.Open("sqlite3", dbLocation)
	db.SetMaxOpenConns(1)
	if err != nil {
		panic(err)
	}
	defer db.Close()

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

func closeDB() {
	if db != nil {
		db.Close()
	}
}

func init() {
	initDB()
}

// defer db.Close()
//   // Create the database tables
//   _, err = db.Exec(`
//     CREATE TABLE mediapool (
//       media_id INTEGER PRIMARY KEY NOT NULL,
//       filename TEXT UNIQUE NOT NULL,
//       mimetype TEXT,
//       size INTEGER,
//       duration REAL
//     )
//   `)
//   if err != nil {
//     panic(err)
//   }

//   _, err = db.Exec(`
//     CREATE TABLE cuesheet (
//       cue_id INTEGER PRIMARY KEY NOT NULL,
//       cuePos INTEGER UNIQUE,
//       cueNum TEXT UNIQUE,
//       media_id INTEGER NOT NULL,
//       title TEXT UNIQUE NOT NULL,
//       posStart INTEGER,
//       posEnd INTEGER,
//       FOREIGN KEY (media_id)
//         REFERENCES mediapool (media_id)
//           ON UPDATE CASCADE
//           ON DELETE CASCADE
//     )
//   `)
//   if err != nil {
//     panic(err)
//   }
// }
