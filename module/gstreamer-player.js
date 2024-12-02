const GStreamer = require('gstreamer-superficial-cf');
const path = require('path')
const config = require('../config')

class GStreamerPlayer {
  constructor() {
    this.pipeline1 = null;
    this.pipeline2 = null;
    this.playlist = [];
    this.currentIndex = 0;
    this.isPaused = false;
    this.isMuted = false;
    this.loopCount = 0;
    this.currentLoop = 0;
    this.fbResolution = {
      width: 1920,
      height: 1080
    }
    this.outputSink = `glimagesink rotate-method="clockwise"`;

    this.outputStage = `
      ! videoscale
      ! video/x-raw,pixel-aspect-ratio=1/1,width=${this.fbResolution.width},height=${this.fbResolution.height}
      ! videoconvert
      ! ${this.outputSink}
    `;

    this.mixStage = `
      compositor name=comp background=transparent
      sink_0::alpha=1
      sink_1::alpha=1
      sink_1::sizing-policy=keep-aspect-ratio
      sink_1::width=${this.fbResolution.width}
      sink_1::height=${this.fbResolution.height}
      ${this.outputStage}

      videotestsrc pattern=2
      ! video/x-raw,framerate=\(fraction\)1/1,
      width=${this.fbResolution.width},
      height=${this.fbResolution.height}
      ! comp.
    `;
  }

  async panic() {
    if (this.pipeline1) {
      console.log("PANIC")
      this.pipeline1.pause();
      await this.pipeline1.stop();
      this.pipeline1 = null;
    }
  }

  async end() {
    if (this.pipeline1) {
      console.log("END")
      this.pipeline1.pause();
      await this.pipeline1.stop();
    }
  }

  async stop() {
    if (this.pipeline1) {
      await this.fadeOut(0.1);
    }
  }

  async showTest(pattern = 0) {
    await this.panic(); // if a pipeline is running kill it

    console.log("SHOW TEST")

    let pipe = `
      ${this.mixStage}
      videotestsrc pattern=${pattern}
      ! comp.
    `;

    this.pipeline1 = new GStreamer.Pipeline(pipe);
    if (this.pipeline1) {
      this.pipeline1.play();
    }
      // Patterns ->
      // smpte (0) – SMPTE 100%% color bars
      // snow (1) – Random (television snow)
      // black (2) – 100%% Black
      // white (3) – 100%% White
      // red (4) – Red
      // green (5) – Green
      // blue (6) – Blue
      // checkers-1 (7) – Checkers 1px
      // checkers-2 (8) – Checkers 2px
      // checkers-4 (9) – Checkers 4px
      // checkers-8 (10) – Checkers 8px
      // circular (11) – Circular
      // blink (12) – Blink
      // smpte75 (13) – SMPTE 75%% color bars
      // zone-plate (14) – Zone plate
      // gamut (15) – Gamut checkers
      // chroma-zone-plate (16) – Chroma zone plate
      // solid-color (17) – Solid color
      // ball (18) – Moving ball
      // smpte100 (19) – SMPTE 100%% color bars
      // bar (20) – Bar
      // pinwheel (21) – Pinwheel
      // spokes (22) – Spokes
      // gradient (23) – Gradient
      // colors (24) – Colors
      // smpte-rp-219 (25) – SMPTE test pattern, RP 219 conformant


  }

  // Function to fade out video to black
  //
  // need to add a duration to the fade
  async fadeOut(duration = 12) {
    if (this.pipeline1) {
      const sink1 = this.pipeline1.getPad('comp', 'sink_1');

      console.log("Start Fade")
      const fps = 25;
      const dfps = 2 * fps;
      const stepLength = 1/dfps * 1000
      const stepSize = 1 / dfps / duration
      for (let alpha = 1; alpha >= 0.0; alpha -= stepSize) {
        sink1.alpha = alpha
        await new Promise(resolve => setTimeout(resolve, stepLength)); // adjust delay as needed
      }
      console.log("STOP")
      this.end();
    }
  };

  async play(filename = null, duration = null) {
    if (filename) {
        await this.load(filename, "replace");
    }

    if (this.pipeline1) {
      console.log("Pipeline loaded")
      this.pipeline1.play();
      this.isPaused = false;
      if (this.loopCount) {
          this.currentLoop = 1;
      }
    }
  }

  async pause() {
      if (this.pipeline1 && !this.isPaused) {
          this.pipeline1.pause();
          this.isPaused = true;
      }
  }

  async togglePause() {
    if (this.pipeline1) {
      if(this.isPaused) {
        this.pipeline1.play()
        this.isPaused = false;
      } else {
        this.pipeline1.pause()
        this.isPaused = true;
      }
    }
  }

  async load(filename, mode = "append") {
    if (mode === "append") {
      this.playlist.push(filename);
    } else {
      this.playlist = [filename];
      this.currentIndex = 0;
    }

    // Load the first file in the pipeline if playlist is empty or replacing
    if (this.playlist.length > 0) {
      await this._loadToPipeline(this.playlist[this.currentIndex]);
    }
  }

  async next() {
    if (this.currentIndex < this.playlist.length - 1) {
      this.currentIndex++;
      await this._loadToPipeline(this.playlist[this.currentIndex]);
      this.play();
    }
  }

  async prev() {
    if (this.currentIndex > 0) {
      this.currentIndex--;
      await this._loadToPipeline(this.playlist[this.currentIndex]);
      this.play();
    }
  }

  async clear() {
    this.playlist = [];
    this.currentIndex = 0;
    this.panic();
  }

  async isPlaying() {
    return !!this.pipeline1 && !this.isPaused;
  }

  async isPaused() {
    return this.isPaused;
  }

  async isMuted() {
    return this.isMuted;
  }

  async getDurationTime() {
    if (this.pipeline1) {
      return this.pipeline1.queryDuration();
    }
    return 0;
  }

  async getRemainingTime() {
    const duration = await this.getDurationTime();
    const position = await this.getPositionTime();
    return duration - position;
  }

  async getPositionTime() {
    if (this.pipeline1) {
      return this.pipeline1.queryPosition();
    }
    return 0;
  }

  async getPositionPercent() {
    const duration = await this.getDuration();
    const position = await this.getPositionTime();
    return (position / duration) * 100;
  }

  async getCurrentPlaying() {
    return this.playlist[this.currentIndex] || null;
  }

  async toggleMute() {
    this.isMuted = !this.isMuted;
    this.pipeline1.mute = this.isMuted;
  }

  async setMute(bool) {
    this.isMuted = bool;
    this.pipeline1.mute = bool;
  }

  async getVolume() {
    return this.pipeline1.volume * 100;
  }

  async setVolume(vol) {
    this.pipeline1.volume = vol / 100;
  }

  async adjustVolume(dB) {
    const currentVol = await this.getVolume();
    this.setVolume(currentVol + dB);
  }

  async setPosition(timeSeconds) {
    if (this.pipeline1) {
      this.pipeline1.seek(timeSeconds * 1000); // Convert seconds to milliseconds
    }
  }

  async loop() {
    this.loopCount = -1;
    this.currentLoop = 1;
  }

  async setLoop(loops) {
    this.loopCount = loops;
    this.currentLoop = 1;
  }

  async clearLoop() {
    this.loopCount = 0;
    this.currentLoop = 0;
  }

  async _loadToPipeline(filename) {
    await this.panic();
    const filePath = path.resolve(filename)

    // const pipe = 'videotestsrc ! capsfilter name=filter ! textoverlay name=text ! fbdevsink'
    // const pipe = "playbin uri=https://www.freedesktop.org/software/gstreamer-sdk/data/media/sintel_trailer-480p.webm"
    const pipe = `
      ${this.mixStage}
      filesrc location="${filePath}"
      ! decodebin
      ! videoconvert
      ! comp.
    `;

    this.pipeline1 = new GStreamer.Pipeline(pipe);
    console.log(`Pipeline1 loaded with ${filename}`)
  }
}

module.exports = GStreamerPlayer;
