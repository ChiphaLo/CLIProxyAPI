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
	hosted := flag.String("hosting-manifest", "", "Explicit verified hosting manifest; independent of shadow mode")
	root := flag.String("shadow-root", "", "Private disposable copied environment; production is unsupported")
	socket := flag.String("socket", "", "Isolated Unix control socket")
	uid := flag.Uint("gui-uid", 995, "Authorized GUI peer UID")
	gid := flag.Int("gui-gid", 986, "Authorized GUI socket group")
	origin := flag.String("candidate-origin", "", "Optional isolated Go candidate origin for configuration transactions")
	controlSocket := flag.String("candidate-control", "", "Optional authenticated private gateway socket")
	controlUID := flag.Uint("candidate-control-uid", 0, "Expected private gateway service UID")
	flag.Parse()
	if *uid > 4294967295 || *gid < 0 || *controlUID > 4294967295 || *origin != "" && *controlSocket != "" {
		return fmt.Errorf("invalid GUI identity")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *hosted != "" {
		if *root != "" || *origin != "" || *controlSocket != "" || *socket != "" {
			return fmt.Errorf("hosted and shadow helper modes are mutually exclusive")
		}
		return serveHosted(ctx, *hosted)
	}
	return serve(ctx, *root, *socket, uint32(*uid), *gid, *origin, *controlSocket, uint32(*controlUID))
}
