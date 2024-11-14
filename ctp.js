const path = require('path')
const fs = require('fs').promises;

const db = require('./db')
const gsp = require('./gsp')
const config = require('./config')

const ctp = {};

ctp.currentCue = -1
ctp.getCue = async (cue_id) => {
  return ctp.currentCue
}
ctp.setCue = async (cue_id) => {
  return ctp.currentCue = cue_id
}
ctp.nextCue = async () => {

}
ctp.prevCue = async () => {

}


ctp.getMediapool = async () => {
  return rows = (await db).all("SELECT * FROM mediapool")
}

ctp.clearCuesheet = async () => {
  return rows = (await db).run("DELETE FROM cuesheet")
}

ctp.getCuesheet = async () => {
  return rows = (await db).all("SELECT * FROM cuesheet LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id")
}

ctp.getPlaylist = async () => {
  const size = await gsp.getProperty("playlist-count")
  let newPlaylist = []
  for (i = 0; i < size; i++) {
    const filePath = await gsp.getProperty(`playlist/${i}/filename`)
    const playlistItem = {
      'id': i,
      'path': filePath,
      'title': path.basename(filePath)
    }
    newPlaylist.push(playlistItem)
  }
  return newPlaylist;
}

ctp.addCue = async (filename) => {
  const row = await (await db).run(
    `INSERT INTO cuesheet (cuePos, cueNum, media_id, title)
    SELECT
        (SELECT COALESCE(MAX(cuePos), 0) + 1 FROM cuesheet) AS cuePos,
        (SELECT COALESCE(MAX(cueNum), 0) + 1 FROM cuesheet) AS cueNum,
        mp.media_id,
        $filename AS title
    FROM
        (SELECT media_id FROM mediapool WHERE filename = $filename) AS mp;`,
    {$filename: filename}
  )

    ctp.currentCue = row.index
    console.log(row.lastID)
  return ctp.currentCue;
}

ctp.removeCue = async (cuePos) => {
  await (await db).run(`
    DELETE FROM cuesheet
    WHERE cuePos = $cuePos;`,
    {$cuePos: cuePos}
  )
  return await (await db).run(`
    UPDATE cuesheet
    SET cuePos = cuePos - 1
    WHERE cuePos > $cuePos;`,
    {$cuePos: cuePos})
}




ctp.upload = async(files) => {
  return (await db).run(`INSERT INTO mediapool (filename,mimetype,size) VALUES (?,?,?)`, ...files );
}
ctp.delete = async(filename) => {
  await (await db).run("DELETE FROM mediapool WHERE filename = ?", [filename]);
  await fs.unlink(path.join(config.app.mediapool, filename));
  console.log(`Deleted ${filename} from Media Pool`)
  return true
}

module.exports = ctp
