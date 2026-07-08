//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package main

func setupIndexReloadSignals(searcher *reloadableSearcher, indexPath string) {
}
