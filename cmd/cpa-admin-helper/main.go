package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("shadow-root", "", "Private disposable copied environment; production is unsupported")
	socket := flag.String("socket", "", "Isolated Unix control socket")
	uid := flag.Uint("gui-uid", 995, "Authorized GUI peer UID")
	gid := flag.Int("gui-gid", 986, "Authorized GUI socket group")
	flag.Parse()
	if *uid > 4294967295 || *gid < 0 {
		return fmt.Errorf("invalid GUI identity")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return serve(ctx, *root, *socket, uint32(*uid), *gid)
}
