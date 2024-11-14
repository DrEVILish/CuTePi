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

  db.serialize(() => {
    //db.run(createTableMediapool);
    //db.run(createTableCuesheet);
  })
  res.send('install')
})

module.exports = router
