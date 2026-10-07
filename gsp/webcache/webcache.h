#ifndef CUTEPI_WEBCACHE_H
#define CUTEPI_WEBCACHE_H

enum { CUTEPI_WK_LIST = 0, CUTEPI_WK_REMOVE = 1, CUTEPI_WK_CLEAR_ALL = 2 };

// One cache operation, started on the WPE thread and polled from Go.
typedef struct {
	int mode;
	char *remove_hosts; // newline-separated: sites to drop (REMOVE)
	char *keep_hosts;   // newline-separated: sites still in use (REMOVE)
	int done;           // set (atomically) when WebKit has replied
	char *names;        // every cached site seen (LIST/REMOVE)
	char *removed;      // the sites dropped (REMOVE)
	char *error;
	int started;        // 1 once handed to WebKit, -1 if that failed
	void *fetched;      // WebKit's record list, held until the remove completes
} cutepi_wk_job;

int cutepi_wk_available(void);
// cutepi_wk_attach runs job on the WPE thread when src (a wpevideosrc)
// creates its web view.
void cutepi_wk_attach(void *src, cutepi_wk_job *job);
int cutepi_wk_done(cutepi_wk_job *job);
int cutepi_wk_started(cutepi_wk_job *job);
void cutepi_wk_job_free(cutepi_wk_job *job);

#endif
