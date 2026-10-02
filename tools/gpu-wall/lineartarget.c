/* lineartarget [card]: can V3D render straight into a linear, scan-out-ready
 * buffer? Creates a 1920x1080 dumb buffer on the display card (no DRM master
 * needed), exports it as a DMABuf, imports it on the render node (EGL
 * surfaceless) as an AB24 linear EGLImage, binds it as a framebuffer, clears
 * it, checks the pixels through the dumb buffer's mapping, then times 600
 * full-screen textured draws into it (glFinish per frame, like a present). */
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
int main(int argc, char **argv) {
  const char *card = argc > 1 ? argv[1] : "/dev/dri/card1";
  int fd = open(card, O_RDWR | O_CLOEXEC);
  if (fd < 0) { perror(card); return 1; }
  struct drm_mode_create_dumb cd = { .width = W, .height = H, .bpp = 32 };
  if (drmIoctl(fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) { perror("create dumb"); return 1; }
  struct drm_mode_map_dumb md = { .handle = cd.handle };
  if (drmIoctl(fd, DRM_IOCTL_MODE_MAP_DUMB, &md)) { perror("map dumb"); return 1; }
  unsigned char *map = mmap(NULL, cd.size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, md.offset);
  if (map == MAP_FAILED) { perror("mmap"); return 1; }
  int dmafd; if (drmPrimeHandleToFD(fd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &dmafd)) { perror("prime export"); return 1; }
  printf("dumb buffer %ux%u pitch %u size %llu, dmabuf fd %d\n", cd.width, cd.height, cd.pitch, (unsigned long long)cd.size, dmafd);

  PFNEGLGETPLATFORMDISPLAYEXTPROC getdpy = (void *)eglGetProcAddress("eglGetPlatformDisplayEXT");
  EGLDisplay dpy = getdpy(EGL_PLATFORM_SURFACELESS_MESA, EGL_DEFAULT_DISPLAY, NULL);
  if (!eglInitialize(dpy, NULL, NULL)) { fprintf(stderr, "eglInitialize failed\n"); return 1; }
  eglBindAPI(EGL_OPENGL_ES_API);
  EGLint cfga[] = { EGL_RENDERABLE_TYPE, EGL_OPENGL_ES2_BIT, EGL_NONE }; EGLConfig cfg; EGLint n;
  eglChooseConfig(dpy, cfga, &cfg, 1, &n);
  EGLint ctxa[] = { EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE };
  EGLContext ctx = eglCreateContext(dpy, n ? cfg : EGL_NO_CONFIG_KHR, EGL_NO_CONTEXT, ctxa);
  if (!eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, ctx)) { fprintf(stderr, "make current failed\n"); return 1; }
  printf("GL_RENDERER %s\n", glGetString(GL_RENDERER));

  EGLint ia[] = { EGL_WIDTH, W, EGL_HEIGHT, H, EGL_LINUX_DRM_FOURCC_EXT, DRM_FORMAT_ABGR8888,
    EGL_DMA_BUF_PLANE0_FD_EXT, dmafd, EGL_DMA_BUF_PLANE0_OFFSET_EXT, 0, EGL_DMA_BUF_PLANE0_PITCH_EXT, (EGLint)cd.pitch,
    EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, 0, EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, 0, EGL_NONE };
  PFNEGLCREATEIMAGEKHRPROC mkimg = (void *)eglGetProcAddress("eglCreateImageKHR");
  EGLImageKHR img = mkimg(dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, ia);
  if (img == EGL_NO_IMAGE_KHR) { fprintf(stderr, "EGLImage import failed: 0x%x\n", eglGetError()); return 1; }
  PFNGLEGLIMAGETARGETTEXTURE2DOESPROC bindimg = (void *)eglGetProcAddress("glEGLImageTargetTexture2DOES");
  GLuint tex; glGenTextures(1, &tex); glBindTexture(GL_TEXTURE_2D, tex); bindimg(GL_TEXTURE_2D, img);
  GLuint fbo; glGenFramebuffers(1, &fbo); glBindFramebuffer(GL_FRAMEBUFFER, fbo);
  glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, tex, 0);
  GLenum st = glCheckFramebufferStatus(GL_FRAMEBUFFER);
  printf("framebuffer on the linear dumb buffer: %s (0x%x)\n", st == GL_FRAMEBUFFER_COMPLETE ? "COMPLETE" : "incomplete", st);
  if (st != GL_FRAMEBUFFER_COMPLETE) return 1;

  glViewport(0, 0, W, H); glClearColor(0.25f, 0.5f, 0.75f, 1.0f); glClear(GL_COLOR_BUFFER_BIT); glFinish();
  unsigned char *px = map + (H / 2) * cd.pitch + (W / 2) * 4;
  printf("after clear, centre pixel bytes R,G,B,A = %u,%u,%u,%u (expect 64,128,191,255)\n", px[0], px[1], px[2], px[3]);

  /* A 1080p source texture (a mixer output stand-in), drawn full screen. */
  unsigned char *src = malloc(W * H * 4);
  for (int y = 0; y < H; y++) for (int x = 0; x < W; x++) { unsigned char *p = src + (y * W + x) * 4; p[0] = x & 255; p[1] = y & 255; p[2] = 200; p[3] = 255; }
  GLuint stex; glGenTextures(1, &stex); glBindTexture(GL_TEXTURE_2D, stex);
  glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, W, H, 0, GL_RGBA, GL_UNSIGNED_BYTE, src);
  glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST); glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
  GLuint prog = glCreateProgram();
  glAttachShader(prog, shader(GL_VERTEX_SHADER, "attribute vec2 p; varying vec2 uv; void main(){ uv = p*0.5+0.5; gl_Position = vec4(p,0.0,1.0); }"));
  glAttachShader(prog, shader(GL_FRAGMENT_SHADER, "precision mediump float; varying vec2 uv; uniform sampler2D t; void main(){ gl_FragColor = texture2D(t, uv); }"));
  glBindAttribLocation(prog, 0, "p"); glLinkProgram(prog); glUseProgram(prog);
  static const GLfloat quad[] = { -1, -1, 1, -1, -1, 1, 1, 1 };
  glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 0, quad); glEnableVertexAttribArray(0);
  glBindFramebuffer(GL_FRAMEBUFFER, fbo);
  double t0 = now(), worst = 0;
  for (int i = 0; i < 600; i++) { double a = now(); glDrawArrays(GL_TRIANGLE_STRIP, 0, 4); glFinish(); double d = now() - a; if (d > worst) worst = d; }
  double t = now() - t0;
  printf("600 full-screen draws into the linear buffer: %.2f ms each on average, worst %.2f ms (%.0f fps)\n", t / 600 * 1000, worst * 1000, 600 / t);
  int x = 700, y = 300; unsigned char *q = map + y * cd.pitch + x * 4, *e = src + ((H - 1 - y) * W + x) * 4;
  printf("pixel (%d,%d) = %u,%u,%u,%u; source %u,%u,%u,%u (rows may be flipped)\n", x, y, q[0], q[1], q[2], q[3], e[0], e[1], e[2], e[3]);
  return 0;
}
