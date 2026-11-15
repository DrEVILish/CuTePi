package gsp

/*
#cgo pkg-config: gobject-2.0 gstreamer-1.0
#include <glib-object.h>
#include <gst/gst.h>
#include <stdlib.h>

// TEMP leak debug: weak refs to retired pipelines, reported with refcounts.
static GPtrArray *cutepi_retired_refs;
static GMutex cutepi_retired_lock;
static void cutepi_retired(gpointer obj) {
	g_mutex_lock(&cutepi_retired_lock);
	if (!cutepi_retired_refs) cutepi_retired_refs = g_ptr_array_new();
	GWeakRef *w = g_new0(GWeakRef, 1);
	g_weak_ref_init(w, obj);
	g_ptr_array_add(cutepi_retired_refs, w);
	g_mutex_unlock(&cutepi_retired_lock);
}
static void cutepi_dump(GString *s, GstElement *e, int depth) {
	g_string_append_printf(s, "%*s%s(%s) rc=%u\n", depth*2, "", GST_OBJECT_NAME(e), G_OBJECT_TYPE_NAME(e), g_atomic_int_get((gint *)&G_OBJECT(e)->ref_count) - 1);
	if (GST_IS_BIN(e)) {
		GST_OBJECT_LOCK(e);
		GList *l = g_list_copy_deep(GST_BIN(e)->children, (GCopyFunc)gst_object_ref, NULL);
		GST_OBJECT_UNLOCK(e);
		for (GList *i = l; i; i = i->next) {
			cutepi_dump(s, i->data, depth+1);
		}
		g_list_free_full(l, gst_object_unref);
	}
}
static char *cutepi_report(void) {
	GString *s = g_string_new(NULL);
	g_mutex_lock(&cutepi_retired_lock);
	for (guint i = 0; cutepi_retired_refs && i < cutepi_retired_refs->len; i++) {
		GWeakRef *w = g_ptr_array_index(cutepi_retired_refs, i);
		GObject *o = g_weak_ref_get(w);
		if (!o) continue;
		cutepi_dump(s, GST_ELEMENT(o), 0);
		g_object_unref(o);
	}
	g_mutex_unlock(&cutepi_retired_lock);
	return g_string_free(s, FALSE);
}

static gint cutepi_alive;
static void cutepi_gone(gpointer data, GObject *obj) { g_atomic_int_add(&cutepi_alive, -1); }
static void cutepi_track(gpointer obj) {
	g_atomic_int_inc(&cutepi_alive);
	g_object_weak_ref(G_OBJECT(obj), cutepi_gone, NULL);
}
static int cutepi_alive_count(void) { return g_atomic_int_get(&cutepi_alive); }
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

// trackAlive counts p until GStreamer finalizes it (a weak ref), so a leaked
// pipeline shows as a count that never comes back down.
func trackAlive(p *gst.Pipeline) { C.cutepi_track(C.gpointer(unsafe.Pointer(p.Instance()))) }

var goCollected, goRetired atomic.Int64

func noteRetired(p *gst.Pipeline) {
	goRetired.Add(1)
	runtime.AddCleanup(p, func(int) { goCollected.Add(1) }, 0)
	C.cutepi_retired(C.gpointer(unsafe.Pointer(p.Instance())))
}

// RetiredReport TEMP leak debug.
func RetiredReport() string {
	c := C.cutepi_report()
	defer C.free(unsafe.Pointer(c))
	return fmt.Sprintf("go wrappers retired %d collected %d\n", goRetired.Load(), goCollected.Load()) + C.GoString(c)
}

// PipelinesAlive is how many cue pipelines built by buildPipeline are not yet
// finalized (playing, warm, standing by, or leaked).
func PipelinesAlive() int { return int(C.cutepi_alive_count()) }
