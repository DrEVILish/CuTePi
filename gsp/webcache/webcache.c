// WebKit cache maintenance for live pages (DESIGN §12.14), reached through
// the WebKitWebView that wpevideosrc hands out in "configure-web-view".
// WebKit is looked up at run time (dlopen), so CuTePi builds without its
// development headers and runs, caching disabled, where it is missing.
// Every call here must run on the WPE thread (the signal's thread): the
// async replies are dispatched on that thread's main context.
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>
#include <glib-object.h>
#include <gio/gio.h>
#include "webcache.h"

// WebKitWebsiteDataTypes: memory and disk HTTP caches only (cookies and
// page storage are the page's own state and stay).
#define WK_CACHE_TYPES ((1 << 0) | (1 << 1))

static void *(*wk_view_session)(void *view);
static void *(*wk_session_manager)(void *session);
static void (*wk_fetch)(void *mgr, int types, GCancellable *c, GAsyncReadyCallback cb, gpointer data);
static GList *(*wk_fetch_finish)(void *mgr, GAsyncResult *res, GError **err);
static void (*wk_remove)(void *mgr, int types, GList *list, GCancellable *c, GAsyncReadyCallback cb, gpointer data);
static gboolean (*wk_remove_finish)(void *mgr, GAsyncResult *res, GError **err);
static void (*wk_clear)(void *mgr, int types, gint64 timespan, GCancellable *c, GAsyncReadyCallback cb, gpointer data);
static gboolean (*wk_clear_finish)(void *mgr, GAsyncResult *res, GError **err);
static const char *(*wk_data_name)(void *data);
static void (*wk_data_unref)(void *data);

int cutepi_wk_available(void) {
	static int state = 0; // 0 unknown, 1 ok, -1 missing
	if (state)
		return state > 0;
	void *h = dlopen("libWPEWebKit-2.0.so.1", RTLD_NOW | RTLD_GLOBAL);
	if (!h) {
		state = -1;
		return 0;
	}
	*(void **)&wk_view_session = dlsym(h, "webkit_web_view_get_network_session");
	*(void **)&wk_session_manager = dlsym(h, "webkit_network_session_get_website_data_manager");
	*(void **)&wk_fetch = dlsym(h, "webkit_website_data_manager_fetch");
	*(void **)&wk_fetch_finish = dlsym(h, "webkit_website_data_manager_fetch_finish");
	*(void **)&wk_remove = dlsym(h, "webkit_website_data_manager_remove");
	*(void **)&wk_remove_finish = dlsym(h, "webkit_website_data_manager_remove_finish");
	*(void **)&wk_clear = dlsym(h, "webkit_website_data_manager_clear");
	*(void **)&wk_clear_finish = dlsym(h, "webkit_website_data_manager_clear_finish");
	*(void **)&wk_data_name = dlsym(h, "webkit_website_data_get_name");
	*(void **)&wk_data_unref = dlsym(h, "webkit_website_data_unref");
	state = (wk_view_session && wk_session_manager && wk_fetch && wk_fetch_finish && wk_remove &&
	         wk_remove_finish && wk_clear && wk_clear_finish && wk_data_name && wk_data_unref) ? 1 : -1;
	return state > 0;
}

void cutepi_wk_job_free(cutepi_wk_job *job) {
	if (!job)
		return;
	g_free(job->remove_hosts);
	g_free(job->keep_hosts);
	g_free(job->names);
	g_free(job->removed);
	g_free(job->error);
	if (job->fetched)
		g_list_free_full(job->fetched, (GDestroyNotify)wk_data_unref);
	g_free(job);
}

// host_matches: a cache record is named by its site; it belongs to host
// when host is the site itself or one of its subdomains.
static int host_matches(const char *host, const char *name) {
	size_t hl = strlen(host), nl = strlen(name);
	if (hl == nl)
		return g_ascii_strcasecmp(host, name) == 0;
	return hl > nl && host[hl - nl - 1] == '.' && g_ascii_strcasecmp(host + hl - nl, name) == 0;
}

// any_matches: hosts is newline-separated.
static int any_matches(const char *hosts, const char *name) {
	if (!hosts || !*hosts)
		return 0;
	gchar **list = g_strsplit(hosts, "\n", -1);
	int hit = 0;
	for (gchar **h = list; *h && !hit; h++)
		hit = **h && host_matches(*h, name);
	g_strfreev(list);
	return hit;
}

static void finish(cutepi_wk_job *job, GError *err) {
	if (err) {
		job->error = g_strdup(err->message);
		g_error_free(err);
	}
	g_atomic_int_set(&job->done, 1);
}

static void on_removed(GObject *mgr, GAsyncResult *res, gpointer data) {
	GError *err = NULL;
	wk_remove_finish(mgr, res, &err);
	finish(data, err);
}

static void on_cleared(GObject *mgr, GAsyncResult *res, gpointer data) {
	GError *err = NULL;
	wk_clear_finish(mgr, res, &err);
	finish(data, err);
}

static void on_fetched(GObject *mgr, GAsyncResult *res, gpointer data) {
	cutepi_wk_job *job = data;
	GError *err = NULL;
	GList *all = wk_fetch_finish(mgr, res, &err);
	if (err) {
		finish(job, err);
		return;
	}
	GString *names = g_string_new(NULL), *removed = g_string_new(NULL);
	GList *drop = NULL;
	for (GList *l = all; l; l = l->next) {
		const char *name = wk_data_name(l->data);
		g_string_append_printf(names, "%s\n", name);
		if (job->mode == CUTEPI_WK_REMOVE && any_matches(job->remove_hosts, name) && !any_matches(job->keep_hosts, name)) {
			drop = g_list_prepend(drop, l->data);
			g_string_append_printf(removed, "%s\n", name);
		}
	}
	job->names = g_string_free(names, FALSE);
	job->removed = g_string_free(removed, FALSE);
	job->fetched = all; // the records stay referenced until the job is freed
	if (drop) {
		wk_remove(mgr, WK_CACHE_TYPES, drop, NULL, on_removed, job);
		g_list_free(drop);
	} else {
		finish(job, NULL);
	}
}

static int start(void *view, cutepi_wk_job *job) {
	if (!cutepi_wk_available())
		return 0;
	void *session = wk_view_session(view);
	void *mgr = session ? wk_session_manager(session) : NULL;
	if (!mgr)
		return 0;
	if (job->mode == CUTEPI_WK_CLEAR_ALL)
		wk_clear(mgr, WK_CACHE_TYPES, 0, NULL, on_cleared, job);
	else
		wk_fetch(mgr, WK_CACHE_TYPES, NULL, on_fetched, job);
	return 1;
}

static void on_configure(GObject *src, GObject *view, gpointer data) {
	cutepi_wk_job *job = data;
	g_atomic_int_set(&job->started, start(view, job) ? 1 : -1);
}

void cutepi_wk_attach(void *src, cutepi_wk_job *job) {
	g_signal_connect(src, "configure-web-view", G_CALLBACK(on_configure), job);
}

int cutepi_wk_done(cutepi_wk_job *job) { return g_atomic_int_get(&job->done); }
int cutepi_wk_started(cutepi_wk_job *job) { return g_atomic_int_get(&job->started); }
