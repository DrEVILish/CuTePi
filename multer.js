const multer = require('multer');
const config = require('./config')

const storage = multer.diskStorage({
  destination: (req, file, cb) => {
    cb(null, config.app.mediapool)
  },
  filename: (req, file, cb) => {
    cb(null, file.originalname)
  }
})
const upload = multer({storage: storage})

module.exports = upload
