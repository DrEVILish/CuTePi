# CuTePi
CuePlayout via Raspberry Pi and mpv, remote HyperDeck Control for ATEM and Companion Integration

- [x] Rename clip

- [ ] Change start point
- [ ] Change end point

- [ ] Drag and Drop YouTube URL into pool, automatically downloads YouTube video as MP4
    - [x] Downloadable YouTube Links

- [ ] Drag and Drop files into MediaPool for upload
    - [x] Drag and Drop upload

- [x] Use mpv to playback images and videos

- [x] Node-mpv has most of the features required.

- [ ] Launch an “mpv —idle” - wasn't able to implement fades
    - [ ] Using gStreamer instead
    - [ ] Implement output scaling
    - [ ] Implement DeckLink SDI output over PCIe/SDI if detected target (Blackmagic Design DeckLink Mini Monitor HD)

- [ ] Must be feature-compatible with HyperDeck playback and UI design. So it works with stream deck.

- [ ] Holding Image - option for after running cue run Holding Image on Loop or Play Next or Play BLACK.png

- [ ] Multiple instances on multiple machines synchronised by websockets.

- [ ] Db is the source of knowledge; the browser doesn't assume state; all UI state is loaded from the server.
