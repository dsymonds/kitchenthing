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
	case <-time.After(3 * time.Second):
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
	if err := p.Init(); err != nil {
		return err
	}

	// TODO: actually loop.
	refresh(ctx, p)
	<-ctx.Done()

	return nil
}

func refresh(ctx context.Context, p paper) {
	// TODO
}
