//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func setupIndexReloadSignals(searcher *reloadableSearcher, indexPath string) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)

	go func() {
		for range signals {
			if err := searcher.Reload(); err != nil {
				fmt.Fprintf(os.Stderr, "reload index %q: %v\n", indexPath, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "reloaded index=%s\n", indexPath)
		}
	}()
}
