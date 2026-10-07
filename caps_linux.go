package main

// The unit grants CAP_NET_BIND_SERVICE as an ambient capability (port 80).
// Ambient capabilities pass on to every program CuTePi starts, and WebKit's
// sandbox (bwrap, started by every live page) refuses to run with
// capabilities it did not expect — the web process then fails and takes
// CuTePi down with it. This constructor clears the ambient set before the Go
// runtime starts its threads: CuTePi keeps the capability in its own
// effective set (it can still bind the port), while the programs it starts
// (WebKit, yt-dlp, nmcli, systemctl) get none.

/*
#include <sys/prctl.h>
#ifndef PR_CAP_AMBIENT
#define PR_CAP_AMBIENT 47
#endif
#ifndef PR_CAP_AMBIENT_CLEAR_ALL
#define PR_CAP_AMBIENT_CLEAR_ALL 4
#endif
__attribute__((constructor)) static void cutepi_clear_ambient_caps(void) {
	prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0);
}
*/
import "C"
