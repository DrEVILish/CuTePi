const express = require('express')
const router = express.Router()
const path = require('path')
const fs = require('fs/promises')

const YTDlpWrap = require('yt-dlp-wrap').default

// Local Modules
const config = require('../config')
const upload = require('../multer')
const ctp = require('../ctp')

const ytdlp = new YTDlpWrap(config.ytdlp.binary)


router.post("/", upload.none(), async (req, res) => {
  const { url } = req.body
  if ( url == "" ){
    res.send("URL Required")
    return
  }
  let metadata = await ytdlp.getVideoInfo(url)
  console.log(metadata)
  const downloadPath = path.join(config.app.mediapool, metadata.title+'.mp4')

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
  .on('close', async () => {
    const size = (await fs.stat(downloadPath)).size
    const files = [[metadata.title+'.mp4', 'video/mp4',size]]
    await ctp.upload(files)
    // if success send updated mediapool, else send fail //

    const mediapool = await ctp.getMediapool();
    res.render('mediapool', { mediapool } )
  })
})

module.exports = router
