const workingDir = "../CTP"

const config = {
  app: {
    port: 3000,
    name: 'CuTePi',
    workingDir: workingDir,
    mediapool: workingDir + "/media"
  },
  db: {
    location: workingDir + "/config/db",
  },
  mpv: {

  },
  ytdlp: {
    binary: "/opt/homebrew/bin/yt-dlp"
  }
};

module.exports = config;
