const config = require('./config');
const path = require('path')
const GStreamerPlayer = require('./module/gstreamer-player');
const gsp = new GStreamerPlayer();

module.exports = gsp
