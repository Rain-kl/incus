package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/egress"
)

func main() {
	restore := flag.Bool("network-restore", false, "restore persisted networking before Incus starts")
	port := flag.Int("network-probe-port", 0, "probe the local SOCKS listener")
	url := flag.String("network-probe-url", "", "HTTPS probe URL")
	dnsAddress := flag.String("network-dns-address", "", "check gateway DNS over TCP and UDP")
	apiPath := flag.String("network-core-api", "", "wait for a local proxy control socket")
	failover := flag.Bool("network-core-failover", false, "initialize the failover balancer to blocked")
	flag.Parse()
	if *apiPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		{
			err := egress.WaitCoreAPI(ctx, *apiPath, *failover)
			if err != nil {
				log.Fatal("proxy startup failed")
			}
		}
		return
	}
	if *restore {
		err := egress.RestoreCLI()
		if err != nil {
			log.Fatal(err)
		}

		return
	}

	if *port > 0 {
		egress.ProbeCLI(*port, *url, *dnsAddress)
		return
	}

	if *dnsAddress != "" {
		egress.ProbeCLI(0, *url, *dnsAddress)
		return
	}
	log.Fatal("expected -network-restore or -network-probe-port")
}
