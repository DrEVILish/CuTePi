package ctp

import (
	"database/sql"
	"log"
	"strconv"

	"CuTePi/gsp"
)

type CTP struct {
	currentCue     int
	cuesheetLength int
}

type Cue struct {
	cuePos   int
	cueNum   string
	media_id int
	title    string
	posStart int
	posEnd   int
	selected bool
}

type Media struct {
	media_id int
	filename string
	mimetype string
	size     int
	duration int
}

type Cuesheet struct {
	cues []Cue
}

type Mediapool struct {
	medias []Media
}

var ctp CTP

func GetCue(cuePos string) (Cue, error) {
	var cue Cue
	err := db.QueryRow(`
		SELECT *
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		WHERE cuePos = :cuePos
	`, sql.Named("cuePos", cuePos)).Scan(&cue.cuePos, &cue.cueNum, &cue.media_id, &cue.title, &cue.posStart, &cue.posEnd, &cue.selected)
	if err != nil {
		return Cue{}, err // Return an empty Cue
	}
	return cue, nil // Return the found Cue
}

func SetCue(cuePos string) (error) {
	var err error
	ctp.currentCue, err = strconv.Atoi(cuePos)
	if err != nil {
		// Handle the error appropriately, e.g., log it or return a default value
		return err
	}
	return nil
}

func NextCue() (error) {
	if ctp.currentCue+1 < ctp.cuesheetLength {
		ctp.currentCue = ctp.currentCue + 1
	}
	return nil
}

func PrevCue() (error) {
	if ctp.currentCue-1 > 1 {
		ctp.currentCue = ctp.currentCue - 1
	}
	return nil
}

func GetCuesheet() (Cuesheet, error) {
	rows, err := db.Query(`
		SELECT *
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
	`)
	if err != nil {
		return Cuesheet{}, err // Handle the error appropriately
	}
	defer rows.Close() // Ensure rows are closed after processing

	var cues []Cue
	for rows.Next() {
		var cue Cue
		if err := rows.Scan(&cue.cuePos, &cue.cueNum, &cue.media_id, &cue.title, &cue.posStart, &cue.posEnd, &cue.selected); err != nil {
			return Cuesheet{}, err
		}
		cues = append(cues, cue)
	}

	cuesheetLength := len(cues)
	if ctp.currentCue > cuesheetLength {
		ctp.currentCue = cuesheetLength
	}

	for i := range cues {
		if cues[i].cuePos == ctp.currentCue {
			cues[i].selected = true
		}
	}
	return Cuesheet{cues}, nil
}

func AddCue(filename string, cuePos string) (error) {
  if (cuePos == "") {
  	_, err := db.Exec(`
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
  	{
     _, err := db.Exec(`
  		UPDATE cuesheet
  		SET cuePos = cuePos + 1
  		WHERE cuePos > ?;
  	`, sql.Named("cuePos", cuePos))
  	if err != nil {
     	log.Printf("Error adding cue: %v", err)
      return err// Log the error instead of panicking
  	}
   }

  	// insert new cue at the new cuePos position
  	{
     _, err := db.Exec(`
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
     	log.Printf("Error adding cue: %v", err)
     return err// Log the error instead of panicking
  	}
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
		return err
	}
	return nil
}

func RemoveCue(filename string) (error) {
	_, err := db.Exec(`
		DELETE FROM cuesheet
		WHERE media_id = (SELECT media_id FROM mediapool WHERE filename = :filename);
	`, sql.Named("filename", filename))
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		DELETE FROM mediapool
		WHERE filename = :filename;
	`, sql.Named("filename", filename))
	if err != nil {
		return err
	}
	return nil
}

func Upload(files []string) {
	for _, file := range files {
		_, err := db.Exec(`
			INSERT INTO mediapool (filename, mimetype, size, duration)
			VALUES (:filename, :mimetype, :size, :duration);
		`,
			sql.Named("filename", file),
			sql.Named("mimetype", gsp.GetMimeType(file)),
			sql.Named("size", gsp.GetFileSize(file)),
			sql.Named("duration", gsp.GetDuration(file)),
		)
		if err != nil {
			panic(err)
		}
	}
}

func Delete(filename string) (error) {
	_, err := db.Exec(`
		DELETE FROM cuesheet
		WHERE media_id = (SELECT media_id FROM mediapool WHERE filename = $1);
	`, filename)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		DELETE FROM mediapool
		WHERE filename = $1;
	`, filename)
	if err != nil {
		return err
	}
	return nil
}

func ClearCueSheet() (error) {
	_, err := db.Exec(`DELETE FROM cuesheet;`)
	if err != nil {
		return err
	}
	return nil
}
