package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	gosync "sync"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
)

var checkMinersFlags struct {
	top uint
}

var checkMinersCmd = &cli.Command{
	Name:        "check-miners",
	Description: "run connectivity checks against miners",
	Action:      runCheckMiners,
	Flags: []cli.Flag{
		&cli.UintFlag{
			Name:        "top",
			Usage:       "only attempt to connect to the top N miners by power",
			Value:       100,
			Destination: &checkMinersFlags.top,
		},
	},
}

type MinersResult struct {
	ResultCommon

	Miner            *address.Address `json:",omitempty"`
	PeerID           *peer.ID         `json:",omitempty"`
	MinerStateMaddrs []string         `json:",omitempty"`
	DHTAddrs         []string         `json:",omitempty"`

	MinerStateDial bool
	DHTLookup      bool
	DHTDial        bool

	Actions []Check `json:",omitempty"`
}

func runCheckMiners(_ *cli.Context) (err error) {
	var (
		wg       gosync.WaitGroup
		ch       = make(chan interface{}, 16)
		filename = fmt.Sprintf("diag.miners.%s.out", time.Now().Format(time.RFC3339))
	)

	log.Infof("writing results to file: %s", filename)

	wg.Add(1)
	go func() {
		defer wg.Done()
		writeReport(filename, ch)
	}()

	ch <- createHeader("miners")

	log.Infow("connecting to bootstrappers", "count", len(bootstrappers))
	connectBootstrappers(ch)

	log.Infow("bootstrapping DHT")
	if err = d.Bootstrap(context.Background()); err != nil {
		log.Fatalf("failed to boostrap DHT: %s", err)
	}

	// load miners from cache.
	var miners []address.Address
	switch file, err := os.Open("miners-cache.data"); {
	case err == nil:
		// cache exists.
		scanner := bufio.NewScanner(file)
		for i := 1; scanner.Scan(); i++ {
			str := strings.Split(scanner.Text(), " ")
			addr, err := address.NewFromString(str[0])
			if err != nil {
				return fmt.Errorf("unparsable address found in cache on line %d: %s; err: %s; aborting", i, str, err)
			}
			miners = append(miners, addr)
		}
		_ = file.Close()

	case os.IsNotExist(err):
		return fmt.Errorf("miners.cache file does not exist; are you running from the git repo directory? aborting")

	default:
		return fmt.Errorf("failed to read miners cache: %w", err)
	}

	// restrict miners to check
	if cnt := checkMinersFlags.top; cnt > 0 {
		miners = miners[:cnt]
	}

	log.Infow("fetching chain head")
	head, err := cl.ChainHead(context.Background())
	if err != nil {
		log.Fatalf("failed to fetch chain head: %s", err)
	}
	log.Infow("fetched chain head", "head", head)

	var tasksWg gosync.WaitGroup
Outer:
	for i, m := range miners {
		minerlog := log.With("miner", m)
		minerlog.Infow("processing miner", "current", i+1, "total", len(miners))

		// fetch the miner's info.
		mi, err := cl.StateMinerInfo(context.Background(), m, head.Key())
		if err != nil {
			ch <- errorMinerResult(m, nil, fmt.Errorf("failed to get miner info: %w", err))
			continue Outer
		}

		minerlog.Infow("fetched miner info", "peer_id", mi.PeerId)

		// convert multiaddrs.
		var addrs []multiaddr.Multiaddr
		for _, addr := range mi.Multiaddrs {
			a, err := multiaddr.NewMultiaddrBytes(addr)
			if err != nil {
				err = fmt.Errorf("failed to interpret multiaddr from miner state; skipping miner: %w", err)
				minerlog.Warnf(err.Error())
				ch <- errorMinerResult(m, &mi, err)
				continue Outer
			}
			addrs = append(addrs, a)
		}

		tasksWg.Add(1)
		go func(m address.Address) {
			defer tasksWg.Done()
			ch <- examineMiner(m, mi.PeerId, addrs)
		}(m)
	}

	tasksWg.Wait()
	close(ch)

	wg.Wait()

	maybeUploadReport(filename)

	return nil
}

func examineMiner(miner address.Address, id *peer.ID, maddrs []multiaddr.Multiaddr) *MinersResult {
	mlog := log.With("miner", miner)

	mlog.Infow("examining miner")
	defer mlog.Infow("done with miner")

	result := &MinersResult{
		ResultCommon:     ResultCommon{Kind: "miner", Timestamp: time.Now()},
		Miner:            &miner,
		PeerID:           id,
		MinerStateMaddrs: stringMaddrs(maddrs),
	}

	// these multiaddrs are non-p2p address, i.e. they do not include peer IDs.
	if len(maddrs) == 0 {
		err := "no miner multiaddrs"
		result.Errors = append(result.Errors, err)
	}

	// we have no peer ID, we can't do a DHT dial nor a direct dial.
	if id == nil {
		err := "no peer id"
		result.Errors = append(result.Errors, err)
		return result
	}

	// try each multiaddr separately.
	for _, ma := range maddrs {
		result.MinerStateDial = true
		ai := peer.AddrInfo{ID: *id, Addrs: []multiaddr.Multiaddr{ma}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		action := dial(ctx, ai, "dial")
		result.Actions = append(result.Actions, action)
		cancel()
	}

	// now let's do a DHT lookup.
	result.DHTLookup = true
	mlog = mlog.With("peer_id", *id)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	action, ai, err := performDHTLookup(ctx, *id, mlog)
	result.Actions = append(result.Actions, action)
	if err != nil {
		return result
	}

	// add the DHT addresses, and mark the fact that we're dialing the DHT addresses.
	result.DHTAddrs = stringMaddrs(ai.Addrs)
	result.DHTDial = true

	// dial the addrinfo returned by the DHT.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	action = dial(ctx, ai, "dht_dial")
	result.Actions = append(result.Actions, action)

	return result
}

func performDHTLookup(ctx context.Context, id peer.ID, peerlog *zap.SugaredLogger) (Check, peer.AddrInfo, error) {
	peerlog.Infow("trying DHT lookup")

	// disconnect from the peer and clear its addresses from the peerstore.
	_ = host.Network().ClosePeer(id)
	host.Peerstore().ClearAddrs(id)

	// perform the DHT lookup.
	start := time.Now()
	addrInfo, err := d.FindPeer(ctx, id)
	took := time.Since(start)

	peerlog.Infow("DHT lookup result", "took", took, "ok", err == nil, "error", err)

	action := Check{
		Kind:    "dht_lookup",
		Success: err == nil,
		Error:   errorMsg(err),
		TookMs:  took.Milliseconds(),
	}
	return action, addrInfo, err
}

func errorMinerResult(m address.Address, mi *miner.MinerInfo, err error) *MinersResult {
	res := &MinersResult{
		ResultCommon: ResultCommon{
			Kind:      "miner",
			Timestamp: time.Now(),
			Errors:    []string{err.Error()},
		},
		Miner: &m,
	}
	if mi != nil {
		res.PeerID = mi.PeerId
	}
	return res
}
