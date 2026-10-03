/* linearsource [card] [frames]: what does it cost to get a 1080p RGBA frame
 * from system memory to the GPU each frame (an alpha layer on the GPU wall)?
 *   a) glTexSubImage2D into an ordinary texture (what glupload does): V3D
 *      tiles the upload on the CPU, in the calling (GL) thread;
 *   b) memcpy into a mapped linear dumb buffer imported as the texture (an
 *      AB24 linear EGLImage): a plain copy, which can run on any thread.
 * For each, per frame: CPU time of the upload/copy, then a full-screen draw
 * sampling it into a 1080p texture with glFinish (GPU time). The sampled
 * pixels are read back to check (b) shows the new frame. Run headless next
 * to the service (no DRM master needed); count V3D buffer creations and TFU
 * jobs from outside with ftrace to see whether Mesa copies (b) behind our
 * back. */
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <GLES2/gl2ext.h>
#include <xf86drm.h>
#include <drm_fourcc.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>
#define W 1920
#define H 1080
static double now(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return t.tv_sec + t.tv_nsec / 1e9; }
static GLuint shader(GLenum type, const char *src) {
  GLuint s = glCreateShader(type); glShaderSource(s, 1, &src, NULL); glCompileShader(s);
  GLint ok; glGetShaderiv(s, GL_COMPILE_STATUS, &ok); if (!ok) { char log[512]; glGetShaderInfoLog(s, 512, NULL, log); fprintf(stderr, "shader: %s\n", log); exit(1); }
  return s;
}
static void fill(unsigned char *dst, int pitch, int frame) {
  for (int y = 0; y < H; y++) { unsigned char *p = dst + (size_t)y * pitch; for (int x = 0; x < W; x++, p += 4) { p[0] = (x + frame) & 255; p[1] = y & 255; p[2] = frame & 255; p[3] = 128; } }
}
int main(int argc, char **argv) {
  const char *card = argc > 1 ? argv[1] : "/dev/dri/card1";
  int N = argc > 2 ? atoi(argv[2]) : 300;
  int fd = open(card, O_RDWR | O_CLOEXEC);
  if (fd < 0) { perror(card); return 1; }
  struct drm_mode_create_dumb cd = { .width = W, .height = H, .bpp = 32 };
  if (drmIoctl(fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) { perror("create dumb"); return 1; }
  struct drm_mode_map_dumb md = { .handle = cd.handle };
  if (drmIoctl(fd, DRM_IOCTL_MODE_MAP_DUMB, &md)) { perror("map dumb"); return 1; }
  unsigned char *map = mmap(NULL, cd.size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, md.offset);
  if (map == MAP_FAILED) { perror("mmap"); return 1; }
  int dmafd; if (drmPrimeHandleToFD(fd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &dmafd)) { perror("prime export"); return 1; }

  PFNEGLGETPLATFORMDISPLAYEXTPROC getdpy = (void *)eglGetProcAddress("eglGetPlatformDisplayEXT");
  EGLDisplay dpy = getdpy(EGL_PLATFORM_SURFACELESS_MESA, EGL_DEFAULT_DISPLAY, NULL);
  if (!eglInitialize(dpy, NULL, NULL)) { fprintf(stderr, "eglInitialize failed\n"); return 1; }
  eglBindAPI(EGL_OPENGL_ES_API);
  EGLint cfga[] = { EGL_RENDERABLE_TYPE, EGL_OPENGL_ES2_BIT, EGL_NONE }; EGLConfig cfg; EGLint n;
  eglChooseConfig(dpy, cfga, &cfg, 1, &n);
  EGLint ctxa[] = { EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE };
  EGLContext ctx = eglCreateContext(dpy, n ? cfg : EGL_NO_CONFIG_KHR, EGL_NO_CONTEXT, ctxa);
  if (!eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, ctx)) { fprintf(stderr, "make current failed\n"); return 1; }

  /* Target: an ordinary 1080p texture (the mixer's output stand-in). */
  GLuint ttex, fbo; glGenTextures(1, &ttex); glBindTexture(GL_TEXTURE_2D, ttex);
  glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, W, H, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
  glGenFramebuffers(1, &fbo); glBindFramebuffer(GL_FRAMEBUFFER, fbo);
  glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, ttex, 0);
  glViewport(0, 0, W, H);

  /* (a) ordinary source texture. */
  GLuint atex; glGenTextures(1, &atex); glBindTexture(GL_TEXTURE_2D, atex);
  glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, W, H, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
  glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR); glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
  /* (b) the linear dumb buffer as the source texture. */
  EGLint ia[] = { EGL_WIDTH, W, EGL_HEIGHT, H, EGL_LINUX_DRM_FOURCC_EXT, DRM_FORMAT_ABGR8888,
    EGL_DMA_BUF_PLANE0_FD_EXT, dmafd, EGL_DMA_BUF_PLANE0_OFFSET_EXT, 0, EGL_DMA_BUF_PLANE0_PITCH_EXT, (EGLint)cd.pitch,
    EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, 0, EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, 0, EGL_NONE };
  PFNEGLCREATEIMAGEKHRPROC mkimg = (void *)eglGetProcAddress("eglCreateImageKHR");
  EGLImageKHR img = mkimg(dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, ia);
  if (img == EGL_NO_IMAGE_KHR) { fprintf(stderr, "EGLImage import failed: 0x%x\n", eglGetError()); return 1; }
  PFNGLEGLIMAGETARGETTEXTURE2DOESPROC bindimg = (void *)eglGetProcAddress("glEGLImageTargetTexture2DOES");
  GLuint btex; glGenTextures(1, &btex); glBindTexture(GL_TEXTURE_2D, btex); bindimg(GL_TEXTURE_2D, img);
  glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR); glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
  printf("source dumb buffer pitch %u; GL_RENDERER %s\n", cd.pitch, glGetString(GL_RENDERER));

  GLuint prog = glCreateProgram();
  glAttachShader(prog, shader(GL_VERTEX_SHADER, "attribute vec2 p; varying vec2 uv; void main(){ uv = p*0.5+0.5; gl_Position = vec4(p,0.0,1.0); }"));
  glAttachShader(prog, shader(GL_FRAGMENT_SHADER, "precision mediump float; varying vec2 uv; uniform sampler2D t; void main(){ gl_FragColor = texture2D(t, uv); }"));
  glBindAttribLocation(prog, 0, "p"); glLinkProgram(prog); glUseProgram(prog);
  static const GLfloat quad[] = { -1, -1, 1, -1, -1, 1, 1, 1 };
  glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 0, quad); glEnableVertexAttribArray(0);

  unsigned char *src = malloc((size_t)W * H * 4);
  for (int mode = 0; mode < 2; mode++) {
    double up = 0, upw = 0, dr = 0, drw = 0;
    int bad = 0;
    for (int i = 0; i < N; i++) {
      fill(src, W * 4, i);
      double a = now();
      if (mode == 0) {
        glBindTexture(GL_TEXTURE_2D, atex);
        glTexSubImage2D(GL_TEXTURE_2D, 0, 0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, src);
      } else {
        for (int y = 0; y < H; y++) memcpy(map + (size_t)y * cd.pitch, src + (size_t)y * W * 4, W * 4);
        glBindTexture(GL_TEXTURE_2D, btex);
      }
      double b = now();
      glDrawArrays(GL_TRIANGLE_STRIP, 0, 4); glFinish();
      double c = now();
      up += b - a; if (b - a > upw) upw = b - a;
      dr += c - b; if (c - b > drw) drw = c - b;
      if (i % 50 == 49) { /* the drawn centre pixel must be this frame's */
        unsigned char px[4]; glReadPixels(W / 2, H / 2, 1, 1, GL_RGBA, GL_UNSIGNED_BYTE, px);
        if (px[2] != (i & 255)) bad++;
      }
    }
    printf("%s: upload/copy %.2f ms (worst %.2f), draw %.2f ms (worst %.2f), stale frames seen %d of %d\n",
      mode ? "b) memcpy into linear dumb buffer" : "a) glTexSubImage2D", up / N * 1000, upw * 1000, dr / N * 1000, drw * 1000, bad, N / 50);
  }
  return 0;
}
