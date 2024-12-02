const express = require('express');
const router = express.Router();

const db = require('../db')

// /install
router.get('/', async (req,res)=>{

  const createTableMediapool = `CREATE TABLE [NOT EXISTS] mediapool (
    media_id INTEGER PRIMARY KEY NOT NULL,
    filename TEXT UNIQUE NOT NULL
    )`;
  const createTableCuesheet = `CREATE TABLE cuesheet (
    cue_id INTEGER PRIMARY KEY NOT NULL,
    media_id INTEGER NOT NULL,
    title TEXT UNIQUE NOT NULL,
    posStart INTEGER,
    posEnd INTEGER,
    FOREIGN KEY (media_id)
      REFERENCES mediapool (media_id)
        ON UPDATE CASCADE
        ON DELETE CASCADE
    )`;

  await (await db).run(createTableCuesheet)
  await (await db).run(createTableMediapool)

  // create Directory /CTP/bin /CTP/media /CTP/config
  // wget yt-dlp (part of bash script app install process)
  // chmod 555 yt-dlp

  res.send('installed')
})

module.exports = router
