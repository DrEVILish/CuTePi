const express = require('express');
const router = express.Router();
const fs = require('fs');
const path = require("path");

const config = require('../config')
const gsp = require('../gsp')
const ctp = require('../ctp')

const zeroPad = (num, places) => String(num).padStart(places, '0')

router.post('/play', async (req, res) => {
  if (gsp.isPlaying) {
    await gsp.play();
  } else if (gsp.isPaused){
    await gsp.togglePause()
  }else {
    await gsp.next();
  }
  res.send("play");
  console.log("API Play")
})

router.post('/togglePause', async (req, res) => {
  await gsp.togglePause();
  if(!gsp.isPaused){
    res.send("resume");
  }else {
    res.send("pause");
  }
})

router.post('/fadeOut', async (req, res) => {
  gsp.fadeOut();
  res.send("fade")
})

router.post('/panic', async (req, res) => {
  await gsp.panic();
  res.send("panic")
})

router.post('/pause', async (req, res) => {
  await gsp.pause();
  res.send("pause");
  console.log("API Pause")
})

router.post('/clear', async (req, res) => {
  await ctp.clearCuesheet()
  const cuesheet = await ctp.getCuesheet()
  res.render('cuesheet', { cuesheet })
})

router.post('/next', async (req, res) => {
  await gsp.next();
  res.send("next");
  console.log("API Next")
})

router.post('/prev', async (req, res) => {
  await gsp.prev();
  res.send("prev");
  console.log("API Prev")
})

router.post('/stop', async (req, res) => {
  gsp.stop();
  res.setHeader('Hx-Trigger', 'cuesheet-updated,')
  res.send("stop");
})

router.post('/test', async (req, res) => {
  gsp.showTest("smpte-rp-219");
  res.send('test');
})

router.post('/play/:filename', async (req, res) => {
  const { filename } = req.params;
  await gsp.load(path.join(config.app.mediapool, filename));
  await gsp.play()
  console.log(`Load '${filename}' and replace`)
  res.send("Play");
})

router.post('/load/:filename', async (req, res) => {
  const { filename } = req.params;
  await gsp.load(path.join(config.app.mediapool, filename),"append");

  console.log(`Load '${filename}' and append`)
  res.setHeader('Hx-Trigger', 'cuesheet-updated')
  res.send("Load");
})

router.post('/add/cue/:filename', async (req, res) => {
  const { filename } = req.params;
  await ctp.addCue(filename)
  const cuesheet = await ctp.getCuesheet()
  res.render('cuesheet', { cuesheet })
})

router.delete('/media/:filename', async (req, res) => {
  const { filename } = req.params;

  if (gsp.currentPlaying == filename) {
    console.log(`Unable to Delete ${filename} playing`)
    res.sendStatus(406);
  } else {
    if (await ctp.delete(filename)){
      res.send();
    }
  }
})

router.delete('/cue/:cuePos', async (req, res) => {
  const { cuePos } = req.params;
  await ctp.removeCue(cuePos)
  const cuesheet = await ctp.getCuesheet()
  res.render('cuesheet', { cuesheet })
})

router.get('/progress', async (req, res) => {

  const timePosition = await gsp.getTimePosition()
  const tPm = Math.floor(timePosition / 60)
  const tPs = Math.round((timePosition - tPm * 60) * 100) / 100

  const timeDuration = await gsp.getDuration()
  const tDm = Math.floor(timeDuration / 60)
  const tDs = Math.round((timeDuration - tDm * 60) * 100) / 100

  const timeRemaining = await gsp.getTimeRemaining()
  const tRm = Math.floor(timeRemaining / 60)
  const tRs = Math.round((timeRemaining - tRm * 60) * 100) / 100

  res.send(`${tPm}:${tPs}/${tDm}:${tDs}  -${tRm}:${tRs}`)

});

module.exports = router;
