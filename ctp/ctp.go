package ctp

import (
  "database/sql"
	"log"
	"strconv"

	"CuTePi/gsp"
)

type CTP struct {
	CurrentCue     int
	CuesheetLength int
}

type Media struct {
	Media_id int     `db:"media_id"`
	Filename string  `db:"filename"`
	Mimetype string  `db:"mimetype"`
	Size     int     `db:"size"`
	Duration int     `db:"duration"`
}

type Cue struct {
  Media
  Cue_id   int    `db:"cue_id"`
 	CuePos   int    `db:"cuePos"`
	CueNum   string `db:"cueNum"`
	Media_id int    `db:"media_id"`
	Title    string `db:"title"`
	PosStart int    `db:"posStart"`
	PosEnd   int    `db:"posEnd"`
	Selected bool
}

type Cuesheet struct {
	Cues []Cue
}

type Mediapool struct {
	Medias []Media
}

var ctp CTP

func GetCue(cuePos string) (cue Cue, err error) {
  query := `
		SELECT *
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		WHERE cuePos = :cuePos
	`
  err = db.Get(&cue, query, sql.Named("cuePos", cuePos))
	if err != nil {
	  log.Printf("Error Getting Cue: %v", err)
		return Cue{}, err // Return an empty Cue
	}
	return cue, nil // Return the found Cue
}

func SetCue(cuePos string) (err error) {
	ctp.CurrentCue, err = strconv.Atoi(cuePos)
	if err != nil {
	  log.Printf("Error setting cue: %v", err)
		// Handle the error appropriately, e.g., log it or return a default value
		return err
	}
	return nil
}

func NextCue() (err error) {
	if ctp.CurrentCue + 1 < ctp.CuesheetLength {
		ctp.CurrentCue = ctp.CurrentCue + 1
	}
	return nil
}

func PrevCue() (err error) {
	if ctp.CurrentCue - 1 > 1 {
		ctp.CurrentCue = ctp.CurrentCue - 1
	}
	return nil
}

func GetCuesheet() (cuesheet Cuesheet, err error) {
  var cues []Cue

	query := `
		SELECT *
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		ORDER BY cuesheet.cuePos
	`

	err = db.Select(&cues, query)
	if err != nil {
	  log.Printf("Error GetCuesheet: %v", err)
		return Cuesheet{}, err
	}

	cuesheetLength := len(cues)
	if ctp.CurrentCue > cuesheetLength {
		ctp.CurrentCue = cuesheetLength
	}

	for i := range cues {
		if cues[i].CuePos == ctp.CurrentCue {
			cues[i].Selected = true
		}
	}
	return Cuesheet{cues}, nil
}

func AddCue(filename string, cuePos string) (err error) {
  if (cuePos == "") {
  	_, err = db.Exec(`
  		INSERT INTO cuesheet (cuePos, cueNum, media_id, title)
  		SELECT
  			(SELECT COALESCE(MAX(cuePos), 0) + 1 FROM cuesheet) AS cuePos,
  			(SELECT COALESCE(MAX(cueNum), 0) + 1 FROM cuesheet) AS cueNum,
  			mp.media_id,
  			:filename AS title
  		FROM
  			(SELECT media_id FROM mediapool WHERE filename = :filename) AS mp;
  	`, sql.Named("filename", filename))
  	if err != nil {
  		log.Printf("Error adding cue: %v", err)
      return err// Log the error instead of panicking
  	}
  } else {
  	// bump every cue up one to make space
    _, err = db.Exec(`
  		UPDATE cuesheet
  		SET cuePos = cuePos + 1
  		WHERE cuePos > ?;
   	`, sql.Named("cuePos", cuePos))
   	if err != nil {
     	log.Printf("Error updating cuePos: %v", err)
      return err// Log the error instead of panicking
   	}


  	// insert new cue at the new cuePos position
     _, err = db.Exec(`
  		INSERT INTO cuesheet (cuePos, cueNum, media_id, title)
  		SELECT
  			:cuePos AS cuePos,
  			(SELECT COALESCE(MAX(cueNum), 0) + 1 FROM cuesheet) AS cueNum,
  			mp.media_id,
  			:filename AS title
  		FROM
  			(SELECT media_id FROM mediapool WHERE filename = :filename) AS mp;
  	`, sql.Named("cuePos", cuePos), sql.Named("filename", filename))
  	if err != nil {
     	log.Printf("Error inserting into cuesheet: %v", err)
     return err// Log the error instead of panicking
  	}
  }
  return nil
}

func UpdateCue(cuePos string, col string, val string) (err error) {
	cuePosInt, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	_,err = db.Exec(`
		UPDATE cuesheet
		SET `+col+` = ?
		WHERE cuePos = ?;`, val, cuePosInt)
	if err != nil {
	  log.Printf("Error updating cue: %v", err)
		return err
	}
	return nil
}

func RemoveCue(filename string) (err error) {
	_, err = db.Exec(`
		DELETE FROM cuesheet
		WHERE media_id = (SELECT media_id FROM mediapool WHERE filename = :filename);
	`, sql.Named("filename", filename))
	if err != nil {
	   log.Printf("Error deleting cue from cuesheet: %v", err)
		return err
	}
	return nil
}

func Upload(files []string) (err error) {
	for _, file := range files {
		_, err = db.Exec(`
			INSERT INTO mediapool (filename, mimetype, size, duration)
			VALUES (:filename, :mimetype, :size, :duration);
		`,
			sql.Named("filename", file),
			sql.Named("mimetype", gsp.GetMimeType(file)),
			sql.Named("size", gsp.GetFileSize(file)),
			sql.Named("duration", gsp.GetDuration(file)),
		)
		if err != nil {
		  log.Printf("Error uploading file: %v", err)
		  return err
		}
	}
	return nil
}

func Delete(filename string) (err error) {
  _, err = db.Exec(`
		DELETE FROM mediapool
		WHERE filename = :filename;
	`, sql.Named("filename", filename))
	if err != nil {
	  log.Printf("Error deleting cue from mediapool: %v", err)
		return err
	}
	return nil
}

func ClearCueSheet() (err error) {
	_, err = db.Exec(`DELETE FROM cuesheet;`)
	if err != nil {
	  log.Printf("Error clearing cuesheet: %v", err)
		return err
	}
	return nil
}
