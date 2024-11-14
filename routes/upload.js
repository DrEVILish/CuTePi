const express = require('express');
const router = express.Router();
const fs = require('fs');

const upload = require('../multer')
const config = require('../config')
const db = require('../db')

const ctp = require('../ctp')

// /upload
router.post("/", upload.array("media"), async (req, res) =>{
  const files = req.files.map((file)=>{
    return [file.filename, file.mimetype, file.size]
  });

  await ctp.upload(files)
  // if success send updated mediapool, else send fail //

  const mediapool = await ctp.getMediapool();
  res.render('mediapool', { mediapool } )
})

// /upload
router.get("/", async (req, res)=>{
  res.render('upload')
})

module.exports = router
