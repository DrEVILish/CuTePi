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

dropzone.addEventListener('click', (e)=>{
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

const upload = document.getElementById("upload");
upload.onclick = (e)=>{
  fileList.innerHTML=""
}
