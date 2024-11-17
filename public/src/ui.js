window.addEventListener("keydown", function(e) {
    if(["Space","ArrowUp","ArrowDown"].indexOf(e.code) > -1) {
        e.preventDefault();
    }
}, false);
