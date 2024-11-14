const express = require('express');
const router = express.Router();
const path = require('path');

const YTDlpWrap = require('yt-dlp-wrap').default;

// Local Modules
const config = require('../config')
const upload = require('../multer')

const ytdlp = new YTDlpWrap(config.ytdlp.binary);


router.post("/", upload.none(), async (req, res) => {
  const { url } = req.body
  if ( url == "" ){
    res.send("URL Required")
    return
  }
  let metadata = await ytdlp.getVideoInfo(url)
  const downloadPath = path.join(config.app.mediapool, metadata.title+'.mp4')
  console.log(downloadPath)
  const download = ytdlp.exec(
    [url, '-f', 'best', '-o', downloadPath]
  )
  .on('ytDlpEvent', (eventType, eventData) => {
    console.log(eventType, eventData)
  })
  .on('error', (error) => {
    console.error(error)
    res.send("download failed")
  })
  .on('close', () => {
    res.setHeader('Hx-Trigger', 'mediapool-updated')
    res.send("download complete")
  })
})

module.exports = router
