package main

import (
	"flag"
	"log"

	"github.com/lxc/incus/v7/internal/server/payincus/egress"
)

func main() {
	restore := flag.Bool("network-restore", false, "restore persisted networking before Incus starts")
	port := flag.Int("network-probe-port", 0, "probe the local SOCKS listener")
	url := flag.String("network-probe-url", "", "HTTPS probe URL")
	flag.Parse()
	if *restore {
		err := egress.RestoreCLI()
		if err != nil {
			log.Fatal(err)
		}

		return
	}

	if *port > 0 {
		egress.ProbeCLI(*port, *url)
		return
	}

	log.Fatal("expected -network-restore or -network-probe-port")
}
