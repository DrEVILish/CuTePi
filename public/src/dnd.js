// Drag-and-drop for the main control page:
//  - OS files dropped onto the media pool pane are uploaded.
//  - Media pool items dragged onto the cuesheet are added as a cue at the
//    hovered drop position, shown with a line indicator.
// Uses plain fetch + DOM replacement (not htmx's own swap machinery) since
// the trigger here is a native drag event, not an hx-trigger; htmx.process
// is called on inserted nodes so their hx-* attributes still work.
(function () {
  function replaceById(id, html) {
    const current = document.getElementById(id);
    if (!current) return;
    const wrapper = document.createElement("div");
    wrapper.innerHTML = html.trim();
    const replacement = wrapper.firstElementChild;
    if (!replacement || replacement.id !== id) {
      console.error("CuTePi: unexpected partial response for #" + id);
      return;
    }
    current.replaceWith(replacement);
    if (window.htmx) {
      htmx.process(replacement);
    }
  }

  function uploadFiles(files) {
    if (!files || files.length === 0) return;
    const formData = new FormData();
    for (const file of files) {
      formData.append("media", file);
    }
    fetch("/upload", {
      method: "POST",
      headers: { "HX-Request": "true" },
      body: formData,
    })
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => replaceById("mediapool", html))
      .catch((err) => console.error("CuTePi: upload failed", err));
  }

  function addCueAt(filename, cuePos, query) {
    let path = cuePos
      ? `/api/cue/add/${encodeURIComponent(filename)}/${encodeURIComponent(cuePos)}`
      : `/api/cue/add/${encodeURIComponent(filename)}`;
    if (query) path += "?" + query;
    fetch(path, { method: "POST" })
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => replaceById("cuesheet", html))
      .catch((err) => console.error("CuTePi: add cue failed", err));
  }

  function setupMediapoolDropzone() {
    const pane = document.getElementById("mediapool-pane");
    if (!pane) return;

    let dragDepth = 0;

    pane.addEventListener("dragenter", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
      dragDepth++;
      pane.classList.add("dnd-dragover");
    });
    pane.addEventListener("dragover", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
    });
    pane.addEventListener("dragleave", () => {
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) pane.classList.remove("dnd-dragover");
    });
    pane.addEventListener("drop", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
      dragDepth = 0;
      pane.classList.remove("dnd-dragover");
      uploadFiles(e.dataTransfer.files);
    });
  }

  function setupMediapoolItemDragSource() {
    document.body.addEventListener("dragstart", (e) => {
      const item = e.target.closest("[data-media-filename]");
      if (!item) return;
      e.dataTransfer.setData("text/plain", item.dataset.mediaFilename);
      e.dataTransfer.effectAllowed = "copy";
    });
  }

  let dropIndicator = null;
  function ensureDropIndicator() {
    if (!dropIndicator) {
      dropIndicator = document.createElement("tr");
      dropIndicator.className = "cue-drop-indicator";
      dropIndicator.innerHTML = "<td colspan=\"6\"></td>";
    }
    return dropIndicator;
  }
  function removeDropIndicator() {
    if (dropIndicator && dropIndicator.parentNode) {
      dropIndicator.parentNode.removeChild(dropIndicator);
    }
    document.querySelectorAll("#cuesheet.dnd-dragover").forEach((sheet) => sheet.classList.remove("dnd-dragover"));
  }

  // Single drop model (§5.4): dragover computes exactly one intent and draws
  // exactly one indicator for it; drop consumes the intent verbatim instead
  // of re-deriving anything from the event target.
  // {mode:"gap"} — reorder line; target read from the line at drop time.
  // {mode:"join-first"|"join-last", groupId} — header drop (§5.4 halves).
  let pendingIntent = null;
  function clearIntent() {
    pendingIntent = null;
    removeDropIndicator();
    document.querySelectorAll(".cue-group-header.dnd-join, .cue-group-header.dnd-join-first").forEach((h) => h.classList.remove("dnd-join", "dnd-join-first"));
  }


  function setupCueReorderDragSource() {
    document.body.addEventListener("dragstart", (e) => {
      // Group header rows: move the whole group block as one unit.
      const groupRow = e.target.closest("tr.cue-group-header");
      if (groupRow && groupRow.dataset.groupId) {
        try {
          e.dataTransfer.setData("application/x-cutepi-group-move", groupRow.dataset.groupId);
          e.dataTransfer.setData("text/x-cutepi-group-move", groupRow.dataset.groupId);
        } catch (err) {}
        e.dataTransfer.effectAllowed = "move";
        groupRow.classList.add("dragging");
        return;
      }
      const row = e.target.closest("tr.cue[data-cue-pos]");
      if (!row) return;
      // Don't hijack media-tile drags (they also bubble but are outside cuesheet)
      // Row drag is distinct: store cuePos in a dedicated mime type.
      const cuePos = row.dataset.cuePos;
      if (!cuePos) return;
      try {
        e.dataTransfer.setData("application/x-cutepi-reorder", cuePos);
        // Fallback plain text for browsers that only expose text/plain in dragover
        e.dataTransfer.setData("text/x-cutepi-reorder", cuePos);
      } catch (err) {}
      e.dataTransfer.effectAllowed = "move";
      row.classList.add("dragging");
    });
    document.body.addEventListener("dragend", (e) => {
      document.querySelectorAll("tr.cue.dragging, tr.cue-group-header.dragging").forEach((r) => r.classList.remove("dragging"));
      removeDropIndicator();
    });
  }

function setupCuesheetDropTarget() {
     document.body.addEventListener("dragover", (e) => {
       const cuesheet = e.target.closest("#cuesheet");
       if (!cuesheet) {
         removeDropIndicator();
         return;
       }
       e.preventDefault();
       const tbody = cuesheet.querySelector("tbody");
       if (!tbody) {
         cuesheet.classList.add("dnd-dragover");
         return;
       }
       cuesheet.classList.remove("dnd-dragover");

       // Clear previous drop target highlights
       tbody.querySelectorAll(".cue-group-header.dnd-drop-target").forEach((h) => h.classList.remove("dnd-drop-target"));

       const rows = Array.from(tbody.querySelectorAll("tr.cue, tr.cue-group-header"));
       const indicator = ensureDropIndicator();
        // Group header bands (§5.4): the near-first strip keeps the plain
        // top-level "between blocks" line; anywhere below the strip JOINs —
        // collapsed folder = last member, expanded header = line right
        // below the header (drops as the FIRST member).
        const HEAD_STRIP = 0.3;
        const isGroupMove = e.dataTransfer.types.includes("application/x-cutepi-group-move") ||
          e.dataTransfer.types.includes("text/x-cutepi-group-move");
        if (!isGroupMove) {
          const hovered = e.target.closest && e.target.closest("tr.cue-group-header");
          if (hovered && hovered.dataset.groupId) {
            const hr = hovered.getBoundingClientRect();
            const frac = hr.height > 0 ? (e.clientY - hr.top) / hr.height : 0.5;
            tbody.querySelectorAll(".cue-group-header.dnd-join, .cue-group-header.dnd-join-first").forEach((h) => h.classList.remove("dnd-join", "dnd-join-first"));
            if (frac > HEAD_STRIP) {
              const gid = parseInt(hovered.dataset.groupId, 10);
              if (hovered.dataset.groupCollapsed === "true") {
                removeDropIndicator();
                hovered.classList.add("dnd-join");
                pendingIntent = { mode: "join-last", groupId: gid };
                return;
              }
              const ind = ensureDropIndicator();
              ind.classList.remove("cue-drop-top");
              ind.classList.add("cue-drop-in");
              ind.style.setProperty("--drop-depth", "1");
              tbody.insertBefore(ind, hovered.nextElementSibling);
              hovered.classList.add("dnd-join-first");
              pendingIntent = { mode: "join-first", groupId: gid };
              return;
            }
            // Top strip: fall through — the line draws above the header,
            // top-level between the blocks.
          }
        }
       tbody.querySelectorAll(".cue-group-header.dnd-join, .cue-group-header.dnd-join-first").forEach((h) => h.classList.remove("dnd-join", "dnd-join-first"));
       // Row bands (§5.4): the line lands in the hovered row's gap and
       // carries that band's membership. Member cue rows indent the line to
       // the member-name level (cue-drop-in) so the jump between "top
       // level" and "inside the group" is unmistakable while dragging.
       let anchor = null;
       let afterRow = false;
       for (const row of rows) {
         const rect = row.getBoundingClientRect();
         if (e.clientY < rect.top + rect.height) {
           anchor = row;
           afterRow = e.clientY >= rect.top + rect.height / 2;
           break;
         }
       }
       const ind = ensureDropIndicator();
       ind.classList.remove("cue-drop-in", "cue-drop-top");
       ind.style.removeProperty("--drop-depth");
       ind.style.removeProperty("padding-left");
       pendingIntent = { mode: "gap" };
       if (anchor) {
         if (anchor.classList.contains("cue-group-header")) {
           anchor.classList.add("dnd-drop-target");
           // On the header body (below the strip): the drop joins — a cue
           // becomes a member, a dragged group nests as a subgroup (§5.4).
           const hr = anchor.getBoundingClientRect();
           const frac = hr.height > 0 ? (e.clientY - hr.top) / hr.height : 0.5;
           if (frac > HEAD_STRIP) {
             const gid = parseInt(anchor.dataset.groupId, 10);
             if (anchor.dataset.groupCollapsed === "true") {
               removeDropIndicator();
               anchor.classList.add("dnd-join");
               pendingIntent = { mode: "join-last", groupId: gid };
             } else {
               const ind2 = ensureDropIndicator();
               ind2.classList.remove("cue-drop-top");
               ind2.classList.add("cue-drop-in");
               ind2.style.setProperty("--drop-depth", "1");
               tbody.insertBefore(ind2, anchor.nextElementSibling);
               anchor.classList.add("dnd-join-first");
               pendingIntent = { mode: "join-first", groupId: gid };
             }
             return;
           }
           // Top-strip line above the header: top-level between blocks.
           pendingIntent = { mode: "cue", drop: { beforeKind: "group", beforeId: parseInt(anchor.dataset.groupId, 10), parent: 0, after: 0 } };
           ind.classList.add("cue-drop-top");
           tbody.insertBefore(ind, anchor);
         } else {
           const gid = parseInt(anchor.dataset.cueGroup, 10) || 0;
           const pos = parseInt(anchor.dataset.cuePos, 10);
           if (gid) {
             // Member band: line indents; the drop joins this group.
             ind.classList.add("cue-drop-in");
             const depth = getComputedStyle(anchor).getPropertyValue("--depth").trim() || "1";
             ind.style.setProperty("--drop-depth", depth);
             pendingIntent = { mode: "cue", drop: { beforeKind: "cue", beforeId: pos, parent: gid, after: afterRow ? pos : 0 } };
           } else {
             pendingIntent = { mode: "cue", drop: { beforeKind: "cue", beforeId: pos, parent: 0, after: afterRow ? pos : 0 } };
           }
           if (afterRow) {
             tbody.insertBefore(ind, anchor.nextElementSibling);
           } else {
             tbody.insertBefore(ind, anchor);
           }
         }
       } else {
         // Empty space below the sheet: always top-level.
         tbody.appendChild(ind);
       }
      });

    document.body.addEventListener("dragend", () => {
      pendingIntent = null;
      removeDropIndicator();
      document.querySelectorAll(".cue-group-header.dnd-join, .cue-group-header.dnd-join-first").forEach((h) => h.classList.remove("dnd-join", "dnd-join-first"));
    });

    document.body.addEventListener("drop", (e) => {
      // Consume the hover intent verbatim: the drop lands exactly where the
      // indicator/highlight showed. Join modes carry their group id; gap
      // mode reads the line position below.
      const intent = pendingIntent || { mode: "gap" };
      pendingIntent = null;
      const joining = intent.mode === "join-last";
      const joiningFirst = intent.mode === "join-first";
      const intentGroupId = intent.groupId != null ? String(intent.groupId) : "";
      document.querySelectorAll(".cue-group-header.dnd-join, .cue-group-header.dnd-join-first").forEach((h) => h.classList.remove("dnd-join", "dnd-join-first"));
      // Show mode locks the sheet: refuse the drop (dragover highlight is
      // harmless, nothing mutates until drop).
      if (document.body.classList.contains("show-mode")) {
        removeDropIndicator();
        return;
      }
      const cuesheet = e.target.closest("#cuesheet");
      if (!cuesheet) return;
      e.preventDefault();
      cuesheet.classList.remove("dnd-dragover");

      // Dropping a cue row directly onto a group header assigns membership.
      // joiningFirst (expanded header, line below it): first cue in group.
      // joining (collapsed highlight): last cue in group. Exception: when the
      // drop line sits immediately ABOVE the header (the hover's upper half
      // drew it there), the intent is "between the blocks" — skip the branch
      // and let the generic reorder path anchor on the line.
      // The header comes from the hover intent, not the drop event target
      // (which may be the indicator row or a child node).
      const header = (joining || joiningFirst) && intentGroupId
        ? document.querySelector(`#cuesheet tr.cue-group-header[data-group-id="${intentGroupId}"]`)
        : null;
      const joinFirst = !!joiningFirst;
      const dropCuePos = (() => {
        try {
          const v = e.dataTransfer.getData("application/x-cutepi-reorder") || e.dataTransfer.getData("text/x-cutepi-reorder");
          return /^\d+$/.test(v) ? v : null;
        } catch (err) { return null; }
      })();
      const dropAboveHeader = header && dropCuePos && dropIndicator && dropIndicator.parentNode &&
        dropIndicator.nextElementSibling === header;
      if (header && !dropAboveHeader) {
        const plain = e.dataTransfer.getData("text/plain");
        if (dropCuePos) {
          removeDropIndicator();
          const form = new FormData();
          form.append("groupId", header.dataset.groupId);
          if (joinFirst) form.append("first", "1");
          fetch("/api/cue/" + dropCuePos + "/group", { method: "POST", body: form })
            .then((res) => {
              if (!res.ok) throw new Error("server returned " + res.status);
              return res.text();
            })
            .then((html) => replaceById("cuesheet", html))
            .catch((err) => console.error("CuTePi: group membership failed", err));
          return;
        }
        if (plain && plain.includes(".")) {
          // Media drop on a header means JOIN: add straight into the group
          // (first member on expanded headers, last on collapsed ones).
          // The old code passed the next row's cuePos, which AddCue lands
          // above the header (outside the group) or after the whole block.
          removeDropIndicator();
          addCueAt(plain, "", "group=" + encodeURIComponent(header.dataset.groupId) + (joinFirst ? "&first=1" : ""));
          return;
        }
        removeDropIndicator();
        return;
      }

      // --- Group block move (drag a group header row) ---
      const dragGroupId = (() => {
        try {
          const v = e.dataTransfer.getData("application/x-cutepi-group-move") || e.dataTransfer.getData("text/x-cutepi-group-move");
          return /^\d+$/.test(v) ? v : null;
        } catch (err) { return null; }
      })();

      // Cue reorder / group move / multi-selection block: ONE literal drop.
      // The server stores the rows at the exact gap shown plus the membership
      // the hovered band carried (§5.4). No re-derivation anywhere.
      if (dropCuePos || dragGroupId) {
        const selected = [...document.querySelectorAll('#cuesheet tr.cue[data-cue-sel="1"], #cuesheet tr.cue[data-cue-anchor="1"]')]
          .map((r) => parseInt(r.dataset.cuePos, 10));
        const body = (() => {
          // Header join intents win: join-first or join-last.
          if ((joining || joiningFirst) && intentGroupId) {
            return { beforeKind: "group", beforeId: parseInt(intentGroupId, 10), join: true, joinFirst: joiningFirst, parent: parseInt(intentGroupId, 10), after: 0 };
          }
          // Band intent from dragover: kind/anchor + membership + slot.
          if (intent && intent.drop) {
            return { beforeKind: intent.drop.beforeKind, beforeId: intent.drop.beforeId, join: false, parent: intent.drop.parent, after: intent.drop.after };
          }
          // No run dragover (keyboard edge) or stale intent: plain end drop.
          return { beforeKind: "end", beforeId: 0, join: false, parent: 0, after: 0 };
        })();
        if (dragGroupId) {
          body.group = parseInt(dragGroupId, 10);
        } else if (selected.includes(parseInt(dropCuePos, 10))) {
          // Dragging a selected row moves the whole block.
          body.cues = selected;
        } else {
          // Dragging an unselected row moves only it (QLab/Finder).
          body.cues = [parseInt(dropCuePos, 10)];
        }
        removeDropIndicator();
        fetch("/api/sheet/drop", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        })
          .then((res) => {
            if (!res.ok) throw new Error("server returned " + res.status);
            return res.text();
          })
          .then((html) => replaceById("cuesheet", html))
          .catch((err) => console.error("CuTePi: drop failed", err));
        return;
      }

      const plain = e.dataTransfer.getData("text/plain");

      const filename = plain;
      let cuePos = null;
      const mediaNext = (dropIndicator && dropIndicator.parentNode) ? dropIndicator.nextElementSibling : null;
      if (mediaNext && mediaNext.dataset.cuePos) {
        cuePos = mediaNext.dataset.cuePos;
      }
      removeDropIndicator();

      if (filename && filename.includes(".")) {
        if (mediaNext && mediaNext.matches && mediaNext.matches("tr.cue-group-header")) {
          // Media dropped on the line above a header: append, then slide
          // the new cue (it lands with the highest position) before that
          // header as a top-level gap.
          const gid = parseInt(mediaNext.dataset.groupId, 10);
          fetch(`/api/cue/add/${encodeURIComponent(filename)}`, { method: "POST" })
            .then((res) => {
              if (!res.ok) throw new Error("server returned " + res.status);
              return res.text();
            })
            .then((html) => {
              replaceById("cuesheet", html);
              const rows = [...document.querySelectorAll('#cuesheet tr.cue[data-cue-pos]')];
              const max = Math.max(...rows.map((r) => parseInt(r.dataset.cuePos, 10)));
              return fetch("/api/sheet/drop", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ cues: [max], beforeKind: "group", beforeId: gid, join: false, parent: 0, after: 0 }),
              });
            })
            .then((res) => {
              if (!res.ok) throw new Error("server returned " + res.status);
              return res.text();
            })
            .then((html) => replaceById("cuesheet", html))
            .catch((err) => console.error("CuTePi: add cue failed", err));
          return;
        }
        // Member-band media line: the ADD carries the band's membership so
        // the new cue lands on the slot the line showed (before mediaNext's
        // position with that group's parent).
        if (intent && intent.drop && intent.drop.parent) {
          addCueAt(filename, cuePos, "group=" + encodeURIComponent(intent.drop.parent));
          return;
        }
        addCueAt(filename, cuePos);
      }
    });
  }

  document.addEventListener("DOMContentLoaded", () => {
    setupMediapoolDropzone();
    setupMediapoolItemDragSource();
    setupCueReorderDragSource();
    setupCuesheetDropTarget();
  });
})();
