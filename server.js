const SegfaultHandler = require('segfault-handler');
SegfaultHandler.registerHandler('crash.log');

const express = require('express');
const path = require('path');

const getIPAddresses = () => {
  const nets = require('os').networkInterfaces();
  const results = {};

  for (const [name, interfaces] of Object.entries(nets)) {
      for (const net of interfaces) {
          const isIPv4 = net.family === 'IPv4' || net.family === 4;
          if (isIPv4 && !net.internal) {
              results[name] ??= [];
              results[name].push(net.address);
          }
      }
  }
  return results;
}


const routes = require('./routes/index');
const api = require('./routes/api');
const install = require('./routes/install');
const upload = require('./routes/upload');
const youtube = require('./routes/youtube');
const config = require('./config');

const app = express();

app.set('views', path.join(__dirname, 'views'));
app.set('view engine', 'pug');

app.use(express.urlencoded({ extended: true }));
app.use(express.static('public'));

app.use('/youtube', youtube);
app.use('/upload', upload);
app.use('/install', install);
app.use('/api', api);
app.use('/', routes);

const server = app.listen(config.app.port, async () => {
  console.log(getIPAddresses())
  console.log(`${config.app.name} listening on port ${config.app.port}`)
});
