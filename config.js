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
  gsp: {
    outputResolutionWidth: 1920,
    outputResolutionHeight: 1080
  },
  ytdlp: {
    binary: "/opt/homebrew/bin/yt-dlp"
  }
};

module.exports = config;
