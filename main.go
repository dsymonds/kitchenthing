package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"
)

var ()

func main() {
	flag.Parse()
	log.Printf("kitchenthing starting...")
	time.Sleep(500 * time.Millisecond)

	p := newPaper()

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())

	// Handle signals.
	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, os.Interrupt) // TODO: others?

		sig := <-sigc
		log.Printf("Caught signal %v; shutting down gracefully", sig)
		cancel()
	}()

	if err := p.Init(); err != nil {
		log.Fatalf("Paper init: %v", err)
	}
	p.DisplayRefresh()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := loop(ctx, p); err != nil {
			log.Printf("Loop failed: %v", err)
		}
		cancel()
	}()

	// Wait a bit. If things are still okay, consider this a successful startup.
	select {
	case <-ctx.Done():
		log.Printf("kitchenthing startup not OK; bailing out")
		goto exit
	case <-time.After(1 * time.Second):
	}

	log.Printf("kitchenthing startup OK")
	time.Sleep(1 * time.Second)

exit:
	<-ctx.Done()
	wg.Wait()
	p.Stop()
	log.Printf("kitchenthing done")
}

func loop(ctx context.Context, p paper) error {
	n := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
		refresh(ctx, p, n)
		n++
		n = n % 80
		if n == 0 {
			p.bw.setAll()
			p.red.clearAll()
		}
	}
}

func refresh(ctx context.Context, p paper, n int) {
	// Each time called, set the first 5n rows black and the first 2n columns red.
	// TODO: something more interesting/useful.
	for row := 5 * n; row < 5*(n+1); row++ {
		for col := 0; col < p.width; col++ {
			p.bw.clear(col, row)
		}
	}
	for col := 2 * n; col < 2*(n+1); col++ {
		for row := 0; row < p.height; row++ {
			p.red.set(col, row)
		}
	}
	p.DisplayRefresh()
}
