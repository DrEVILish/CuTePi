const dropzone = document.getElementById('dropzone');
const fileInput = document.getElementById('file-input');
const form = document.getElementById('dropform');
const fileList = document.getElementById("filelist");

var isAdvancedUpload = function() {
  var div = document.createElement('div');
  return (('draggable' in div) || ('ondragstart' in div && 'ondrop' in div)) && 'FormData' in window && 'FileReader' in window;
}();

if (isAdvancedUpload) {
  dropzone.classList.add('has-advanced-upload');

  var droppedFiles = new DataTransfer();
}


function preventDefaults(e) {
  e.preventDefault();
  e.stopPropagation();
}

// The label already opens the picker natively; opening it a second time from
// here made iOS Safari drop the first picker and, with it, the chosen files.
dropzone.addEventListener('click', (e)=>{
  if (e.target.closest("label, input")) return;
  fileInput.click();
})
dropzone.addEventListener('dragover', (e)=>{
  preventDefaults(e);
  e.dataTransfer.dropEffect = 'copy'
  dropzone.classList.add('drag-over');
});
dropzone.addEventListener('dragenter', preventDefaults);
dropzone.addEventListener('dragleave', (e) => {
  preventDefaults(e)
  dropzone.classList.remove('drag-over');
});

dropzone.addEventListener('drop', (e)=>{
  preventDefaults(e);
  const files = e.target.files || (e.dataTransfer && e.dataTransfer.files);

  for (var i = 0; i < files.length; i++) {
    //Add files to the selected file list
    droppedFiles.items.add(files[i]);
    const li = document.createElement("li");
    const name = document.createTextNode(files[i].name);
    li.appendChild(name);
    fileList.appendChild(li);
  }

  // Checking if there are any files
  if (droppedFiles.files.length) {
    // Assigning the files to the hidden input from the first step
    fileInput.files = droppedFiles.files;
  }
})

// Also reflect files chosen via the OS picker (the standalone /upload page
// relies on the picker; the modal does the same). Additive on top of the
// drag-and-drop path above.
fileInput.addEventListener("change", () => {
  fileList.innerHTML = "";
  for (const f of fileInput.files) {
    const li = document.createElement("li");
    li.appendChild(document.createTextNode(f.name));
    fileList.appendChild(li);
  }
});

// XHR upload with live progress + success/failure feedback (both this modal
// and the standalone /upload page share the same markup ids). htmx is not
// involved in the submission, so its early "looks like nothing happened"
// behaviour is gone; the status line carries the operator-facing feedback.
const uploadProgress = document.getElementById("progress");
const uploadStatus = document.getElementById("upload-status");

function setUploaderFeedback(text, cls) {
  if (uploadStatus) {
    uploadStatus.textContent = text;
    uploadStatus.className = "small " + (cls || "");
  }
}

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const files = fileInput.files;
  if (!files || files.length === 0) {
    setUploaderFeedback("No file chosen.", "text-warning");
    return;
  }
  // Warn before sending more than the media disk can hold (§5.7).
  if (window.cutepiCheckSpace && !(await window.cutepiCheckSpace(Array.from(files)))) {
    setUploaderFeedback("Upload cancelled — not enough disk space.", "text-warning");
    return;
  }
  const choice = window.cutepiUploadChoice ? await window.cutepiUploadChoice(files) : { go: true, onConflict: "" };
  if (!choice.go) {
    setUploaderFeedback("Upload cancelled.", "text-warning");
    return;
  }
  const formData = new FormData();
  if (choice.onConflict) formData.append("onConflict", choice.onConflict);
  for (const f of files) formData.append("media", f);
  const btn = document.getElementById("upload");
  if (btn) { btn.disabled = true; btn.classList.add("disabled"); }
  if (uploadProgress) { uploadProgress.hidden = false; uploadProgress.value = 0; }
  setUploaderFeedback("Uploading… 0%");

  const xhr = new XMLHttpRequest();
  xhr.open("POST", "/upload");
  // Same header the fetch path uses; the server returns the #mediapool partial.
  xhr.setRequestHeader("HX-Request", "true");
  xhr.upload.addEventListener("progress", (ev) => {
    if (!ev.lengthComputable || !uploadProgress) return;
    const pct = Math.round((ev.loaded / ev.total) * 100);
    uploadProgress.value = pct;
    setUploaderFeedback("Uploading… " + pct + "%");
  });
  // Bytes are all sent: the server now probes/validates each file before it
  // joins the pool, so show an indeterminate bar instead of a stuck 100%.
  xhr.upload.addEventListener("load", () => {
    if (uploadProgress) uploadProgress.removeAttribute("value");
    setUploaderFeedback("Importing… validating media on the server");
  });
  xhr.addEventListener("load", () => {
    if (btn) { btn.disabled = false; btn.classList.remove("disabled"); }
    if (uploadProgress) uploadProgress.value = xhr.status >= 200 && xhr.status < 300 ? 100 : 0;
    if (xhr.status >= 200 && xhr.status < 300) {
      const summary = window.cutepiUploadSummary ? window.cutepiUploadSummary(xhr.getResponseHeader("X-Upload-Result")) : "Upload complete";
      setUploaderFeedback(summary, "text-success");
      fileList.innerHTML = "";
      fileInput.value = "";
      if (droppedFiles && droppedFiles.items) droppedFiles = new DataTransfer();
      // The partial is a standalone #mediapool; splice it in when present
      // (the control centre modal). The standalone /upload page just keeps
      // the success message.
      if (document.getElementById("mediapool")) {
        const wrapper = document.createElement("div");
        wrapper.innerHTML = (xhr.responseText || "").trim();
        const replacement = wrapper.firstElementChild;
        const current = document.getElementById("mediapool");
        if (replacement && replacement.id === "mediapool" && current) {
          current.replaceWith(replacement);
          if (window.htmx) htmx.process(replacement);
        }
      }
      if (typeof showToast === "function") showToast(summary, "success");
      if (window.bootstrap && document.getElementById("uploadModal")) {
        try { bootstrap.Modal.getInstance(document.getElementById("uploadModal")).hide(); } catch (err) {}
      }
    } else {
      let reason = "";
      try {
        const j = JSON.parse(xhr.responseText);
        if (j && j.error) reason = j.error;
      } catch (err) { /* not JSON */ }
      // htmx requests get the reason as plain text; a full error.html page
      // (non-htmx fallback) keeps it in its <pre>.
      const body = (xhr.responseText || "").trim();
      if (!reason && !body.startsWith("<")) reason = body.replace(/\s+/g, " ").slice(0, 300);
      if (!reason) try {
        const doc = new DOMParser().parseFromString(body, "text/html");
        reason = (doc.querySelector("pre") || doc.body).textContent.replace(/\s+/g, " ").trim().slice(0, 300);
      } catch (err) { /* status alone still shows */ }
      setUploaderFeedback("Upload failed (" + xhr.status + ")" + (reason ? ": " + reason : ""), "text-danger");
    }
  });
  xhr.addEventListener("error", () => {
    if (btn) { btn.disabled = false; btn.classList.remove("disabled"); }
    setUploaderFeedback("Upload failed (network error).", "text-danger");
  });
  xhr.send(formData);
});
