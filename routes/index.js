const express = require('express');
const router = express.Router();
const fs = require('fs');
const path = require('path');

// Local Modules
const config = require('../config')

const gsp = require('../gsp')
const db = require('../db')
const ctp = require('../ctp')

router.put('/mediapool', async (req,res)=>{
  res.setHeader('Hx-Trigger', 'mediapool-updated')
  res.send("update")
})

router.get('/mediapool', async (req,res)=>{
  const mediapool = await ctp.getMediapool()
  res.render('mediapool', { mediapool } )
})

router.all("/cuesheet", async (req, res)=>{
  const cuesheet = await ctp.getCuesheet()
  res.render('cuesheet', { cuesheet })
})

router.get('/', async (req,res)=>{
  const mediapool = await ctp.getMediapool()
  const cuesheet = await ctp.getCuesheet()
  res.render('index', { cuesheet, mediapool } )
})

module.exports = router;
