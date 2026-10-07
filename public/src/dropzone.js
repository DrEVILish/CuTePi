const dropzone = document.getElementById('dropzone');
const fileInput = document.getElementById('file-input');
const form = document.getElementById('dropform');
const fileList = document.getElementById("filelist");

dropzone.classList.add('has-advanced-upload');
let droppedFiles = new DataTransfer();

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

// Upload with live progress + success/failure feedback (both this modal and
// the standalone /upload page share the same markup ids), through ui.js's
// upload helpers like the pool drop. htmx is not involved in the submission;
// the status line carries the operator-facing feedback.
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
  if (!(await cutepiCheckSpace(Array.from(files)))) {
    setUploaderFeedback("Upload cancelled — not enough disk space.", "text-warning");
    return;
  }
  const choice = await cutepiUploadChoice(files);
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

  let res;
  try {
    // pct null: bytes are all sent and the server is probing/validating each
    // file, so the bar goes indeterminate instead of sticking at 100%.
    res = await cutepiUpload("/upload", formData, (pct, text) => {
      if (uploadProgress) {
        if (pct === null) uploadProgress.removeAttribute("value"); else uploadProgress.value = pct;
      }
      setUploaderFeedback(text);
    });
  } catch (err) {
    setUploaderFeedback("Upload failed (network error).", "text-danger");
    return;
  } finally {
    if (btn) { btn.disabled = false; btn.classList.remove("disabled"); }
  }
  const ok = res.status >= 200 && res.status < 300;
  if (uploadProgress) uploadProgress.value = ok ? 100 : 0;
  if (!ok) {
    setUploaderFeedback("Upload failed (" + res.status + "): " + uploadErrorText(res.status, res.text), "text-danger");
    return;
  }
  const summary = cutepiUploadSummary(res.result);
  setUploaderFeedback(summary, "text-success");
  fileList.innerHTML = "";
  fileInput.value = "";
  droppedFiles = new DataTransfer();
  // The partial is a standalone #mediapool; splice it in when present (the
  // control centre modal). The standalone /upload page just keeps the
  // success message.
  if (document.getElementById("mediapool")) replaceById("mediapool", res.text);
  showToast(summary, "success");
  hideModal("uploadModal");
});
