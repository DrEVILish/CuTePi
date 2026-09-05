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

  function addCueAt(filename, cuePos) {
    const path = cuePos
      ? `/api/cue/add/${encodeURIComponent(filename)}/${encodeURIComponent(cuePos)}`
      : `/api/cue/add/${encodeURIComponent(filename)}`;
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


  function setupCueReorderDragSource() {
    document.body.addEventListener("dragstart", (e) => {
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
      document.querySelectorAll("tr.cue.dragging").forEach((r) => r.classList.remove("dragging"));
      removeDropIndicator();
    });
  }

  function reorderCues(newOrder) {
    fetch("/api/cue/reorder", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ order: newOrder }),
    })
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => replaceById("cuesheet", html))
      .catch((err) => console.error("CuTePi: reorder failed", err));
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

      const rows = Array.from(tbody.querySelectorAll("tr.cue"));
      const indicator = ensureDropIndicator();
      let target = null;
      for (const row of rows) {
        const rect = row.getBoundingClientRect();
        if (e.clientY < rect.top + rect.height / 2) {
          target = row;
          break;
        }
      }
      if (target) {
        tbody.insertBefore(indicator, target);
      } else {
        tbody.appendChild(indicator);
      }
    });

    document.body.addEventListener("dragend", removeDropIndicator);

    document.body.addEventListener("drop", (e) => {
      const cuesheet = e.target.closest("#cuesheet");
      if (!cuesheet) return;
      e.preventDefault();
      cuesheet.classList.remove("dnd-dragover");

      // Check for cue reorder first (distinct mime type, higher priority than media add)
      let reorderPos = null;
      try {
        reorderPos = e.dataTransfer.getData("application/x-cutepi-reorder") || e.dataTransfer.getData("text/x-cutepi-reorder");
      } catch (err) {}
      // Fallback: text/plain that looks like a cuePos and originated from a cue row drag
      // (media filenames contain a dot, cuePos is numeric only)
      const plain = e.dataTransfer.getData("text/plain");
      if (reorderPos && /^\d+$/.test(reorderPos)) {
        // Build the new order: existing cuePos values with the dragged one moved to the indicator position
        const tbody = cuesheet.querySelector("tbody");
        if (!tbody) {
          removeDropIndicator();
          return;
        }
        const rows = Array.from(tbody.querySelectorAll("tr.cue"));
        const existing = rows.map((r) => r.dataset.cuePos);
        const dragged = reorderPos;
        // Determine insertion index from indicator position
        let insertIdx = existing.length;
        if (dropIndicator && dropIndicator.parentNode) {
          const nextRow = dropIndicator.nextElementSibling;
          if (nextRow && nextRow.dataset.cuePos) {
            insertIdx = existing.indexOf(nextRow.dataset.cuePos);
            if (insertIdx === -1) insertIdx = existing.length;
          }
        }
        // Remove dragged from existing, then insert at new position
        const filtered = existing.filter((p) => p !== dragged);
        // If dragged was not in existing (should not happen), just use plain reorder detection fallback to media add
        if (filtered.length !== existing.length - 1 && !existing.includes(dragged)) {
          removeDropIndicator();
          // Fall through to media add path
        } else {
          // Adjust insertIdx if removal shifted it (when dragged was before insert position)
          const originalIdx = existing.indexOf(dragged);
          if (originalIdx !== -1 && originalIdx < insertIdx) insertIdx--;
          filtered.splice(insertIdx, 0, dragged);
          const newOrder = filtered.map((s) => parseInt(s, 10));
          removeDropIndicator();
          // Only send if order actually changed
          const isSame = newOrder.length === existing.length && newOrder.every((v, i) => String(v) === existing[i]);
          if (!isSame) reorderCues(newOrder);
          else removeDropIndicator();
          return;
        }
      }

      const filename = plain;
      let cuePos = null;
      if (dropIndicator && dropIndicator.parentNode) {
        const nextRow = dropIndicator.nextElementSibling;
        if (nextRow && nextRow.dataset.cuePos) {
          cuePos = nextRow.dataset.cuePos;
        }
      }
      removeDropIndicator();

      if (filename && filename.includes(".")) {
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
