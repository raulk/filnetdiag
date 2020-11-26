package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/filecoin-project/lotus/lib/addrutil"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
)

var bootstrappers = func() []peer.AddrInfo {
	// parse bootstrapper addresses.
	addrs, err := addrutil.ParseAddresses(context.Background(), []string{
		"/dns4/bootstrap-0.mainnet.filops.net/tcp/1347/p2p/12D3KooWCVe8MmsEMes2FzgTpt9fXtmCY7wrq91GRiaC8PHSCCBj",
		"/dns4/bootstrap-1.mainnet.filops.net/tcp/1347/p2p/12D3KooWCwevHg1yLCvktf2nvLu7L9894mcrJR4MsBCcm4syShVc",
		"/dns4/bootstrap-2.mainnet.filops.net/tcp/1347/p2p/12D3KooWEWVwHGn2yR36gKLozmb4YjDJGerotAPGxmdWZx2nxMC4",
		"/dns4/bootstrap-3.mainnet.filops.net/tcp/1347/p2p/12D3KooWKhgq8c7NQ9iGjbyK7v7phXvG6492HQfiDaGHLHLQjk7R",
		"/dns4/bootstrap-4.mainnet.filops.net/tcp/1347/p2p/12D3KooWL6PsFNPhYftrJzGgF5U18hFoaVhfGk7xwzD8yVrHJ3Uc",
		"/dns4/bootstrap-5.mainnet.filops.net/tcp/1347/p2p/12D3KooWLFynvDQiUpXoHroV1YxKHhPJgysQGH2k3ZGwtWzR4dFH",
		"/dns4/bootstrap-6.mainnet.filops.net/tcp/1347/p2p/12D3KooWP5MwCiqdMETF9ub1P3MbCvQCcfconnYHbWg6sUJcDRQQ",
		"/dns4/bootstrap-7.mainnet.filops.net/tcp/1347/p2p/12D3KooWRs3aY1p3juFjPy8gPN95PEQChm2QKGUCAdcDCC4EBMKf",
		"/dns4/bootstrap-8.mainnet.filops.net/tcp/1347/p2p/12D3KooWScFR7385LTyR4zU1bYdzSiiAb5rnNABfVahPvVSzyTkR",
		"/dns4/lotus-bootstrap.forceup.cn/tcp/41778/p2p/12D3KooWFQsv3nRMUevZNWWsY1Wu6NUzUbawnWU5NcRhgKuJA37C",
		"/dns4/bootstrap-0.starpool.in/tcp/12757/p2p/12D3KooWGHpBMeZbestVEWkfdnC9u7p6uFHXL1n7m1ZBqsEmiUzz",
		"/dns4/bootstrap-1.starpool.in/tcp/12757/p2p/12D3KooWQZrGH1PxSNZPum99M1zNvjNFM33d1AAu5DcvdHptuU7u",
		"/dns4/node.glif.io/tcp/1235/p2p/12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt",
		"/dns4/bootstrap-0.ipfsmain.cn/tcp/34721/p2p/12D3KooWQnwEGNqcM2nAcPtRR9rAX8Hrg4k9kJLCHoTR5chJfz6d",
		"/dns4/bootstrap-1.ipfsmain.cn/tcp/34723/p2p/12D3KooWMKxMkD5DMpSWsW7dBddKxKT7L2GgbNuckz9otxvkvByP",
	})
	if err != nil {
		log.Fatalf("failed to parse addresses: %s", err)
	}
	return addrs
}()

var probeBootstrappersCmd = &cli.Command{
	Name:        "probe-bootstrappers",
	Description: "run connectivity checks against bootstrappers",
	Action:      runProbeBootstrappers,
}

type BootstrapperResult struct {
	ResultCommon
	PeerID  *peer.ID              `json:",omitempty"`
	Addrs   []multiaddr.Multiaddr `json:",omitempty"`
	Actions []Action              `json:",omitempty"`
}

func runProbeBootstrappers(_ *cli.Context) error {
	var (
		wg       sync.WaitGroup
		ch       = make(chan interface{}, 16)
		filename = fmt.Sprintf("diag.bootstrappers.%s.out", time.Now().Format(time.RFC3339))
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		writeResults(filename, ch)
	}()

	log.Infow("connecting to bootstrappers", "count", len(bootstrappers))
	connectBootstrappers(ch)
	close(ch)

	return nil
}

func connectBootstrappers(ch chan interface{}) {
	for _, ai := range bootstrappers {
		ai := ai

		log.Infow("connecting to bootstrapper", "id", ai.ID)
		_ = host.Network().ClosePeer(ai.ID)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := host.Connect(ctx, ai)
		cancel()

		took := time.Since(start)
		log.Infow("bootstrapper connection result", "id", ai.ID, "took", took, "ok", err == nil, "error", err)

		if ch != nil {
			ch <- &BootstrapperResult{
				ResultCommon: ResultCommon{
					Kind:      "bootstrapper",
					Timestamp: time.Now(),
				},
				PeerID: &ai.ID,
				Addrs:  ai.Addrs,
				Actions: []Action{{
					Kind:    "dial",
					Success: err == nil,
					Error:   errorMsg(err),
					Latency: took.Milliseconds(),
				}},
			}
		}
	}
}
