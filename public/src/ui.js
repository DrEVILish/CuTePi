window.addEventListener("keydown", (e) => {
  if (document.activeElement.nodeName.toLowerCase() != "input") {
    if(["Space","ArrowUp","ArrowDown"].indexOf(e.code) > -1) {
      e.preventDefault();
    }
    if (["Space"].indexOf(e.code) > -1) {
      htmx.trigger("#spaceBar", "spaceBar")
    }
    if (["ArrowUp"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowUp", "ArrowUp")
    }
    if (["ArrowDown"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowDown", "ArrowDown")
    }
    if (["Escape"].indexOf(e.code) > -1) {
      htmx.trigger("#esc", "esc")
    }
  }
}, false);
