// Package monitor implements filesystem monitoring via fsnotify/inotify.
// It watches indexed directories for changes (create, delete, modify, rename)
// and updates the LMDB store and in-memory indexes in real time.
//
// The monitor handles inotify event queue overflow (IN_Q_OVERFLOW) by
// triggering a reconciliation crawl, and performs periodic reconciliation
// as a safety net for missed events.
package monitor
