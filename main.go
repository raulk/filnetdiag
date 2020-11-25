package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/lib/addrutil"
	core "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/ratelimit"
	"go.uber.org/zap"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/sync"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	record "github.com/libp2p/go-libp2p-record"
)

var bootstrappers = []string{
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
}

var (
	cl     client
	head   *types.TipSet
	miners []address.Address
	host   core.Host
	d      *dht.IpfsDHT
	wg     gosync.WaitGroup
	log    *zap.SugaredLogger
)

type client struct {
	ChainHead        func(context.Context) (*types.TipSet, error)
	StateListMiners  func(context.Context, types.TipSetKey) ([]address.Address, error)
	StateMinerInfo   func(context.Context, address.Address, types.TipSetKey) (miner.MinerInfo, error)
	StateNetworkName func(context.Context) (dtypes.NetworkName, error)
}

type Action struct {
	Kind    string        `json:",omitempty"` // "direct", "dht"
	Success bool          `json:",omitempty"`
	Error   error         `json:",omitempty"`
	Latency time.Duration `json:",omitempty"`
}

type Result struct {
	Timestamp    time.Time             `json:",omitempty"`
	Bootstrapper bool                  `json:",omitempty"`
	Miner        address.Address       `json:",omitempty"`
	PeerID       *peer.ID              `json:",omitempty"`
	Error        error                 `json:",omitempty"`
	Notes        []string              `json:",omitempty"`
	Addrs        []multiaddr.Multiaddr `json:",omitempty"`
	DHTAddrs     []multiaddr.Multiaddr `json:",omitempty"`
	WasConnected bool                  `json:",omitempty"`
	TriedDHT     bool                  `json:",omitempty"`
	Actions      []Action              `json:",omitempty"`
}

func main() {
	l, _ := zap.NewProduction(zap.WithCaller(false))
	log = l.Sugar()
	defer log.Sync()

	// get all miners from glif.
	closer, err := jsonrpc.NewClient(context.Background(), "https://api.node.glif.io", "Filecoin", &cl, nil)
	if err != nil {
		log.Fatalf("failed to create JSON-RPC client: %s; aborting", err)
	}
	defer closer()

	// construct the host.
	host, err = constructLibp2pHost()
	if err != nil {
		log.Fatalf("failed to instantiate libp2p host: %s; aborting", err)
	}

	// construct the DHT.
	d, err = constructDHT()
	if err != nil {
		log.Fatalf("failed to construct DHT: %s; aborting", err)
	}

	var ch = make(chan *Result, 16)
	wg.Add(1)
	go processResults(ch)

	log.Infow("connecting to bootstrappers", "count", len(bootstrappers))
	connectBootstrappers(ch)

	log.Infow("bootstrapping DHT")

	if err = d.Bootstrap(context.Background()); err != nil {
		log.Fatalf("failed to boostrap DHT: %s", err)
	}

	head, err = cl.ChainHead(context.Background())
	if err != nil {
		log.Fatalf("failed to obtain chain head: %s; aborting", err)
	}

	log.Infow("fetching miners from chain head", "head", head)
	miners, err = cl.StateListMiners(context.Background(), head.Key())
	if err != nil {
		log.Fatalf("failed to list miners in network: %s; aborting", err)
	}

	log.Infow("obtained miners", "count", len(miners))

	var (
		wg   gosync.WaitGroup
		once atomic.Value // gosync.Once
	)

	once.Store(new(gosync.Once))
	ctx, cancel := context.WithCancel(context.Background())
	// renew the once barrier every 20 seconds to fetch the newest head.
	cycleHead := func() {
		defer wg.Done()
		for {
			select {
			case <-time.After(20 * time.Second):
				once.Store(new(gosync.Once))
			case <-ctx.Done():
				return
			}
		}
	}

	wg.Add(1)
	go cycleHead()

	// apply a rate limit of 2 peers per second.
	rl := ratelimit.New(2, ratelimit.WithoutSlack)

	var tasksWg gosync.WaitGroup
Outer:
	for i, m := range miners {
		minerlog := log.With("miner", m)

		rl.Take()

		minerlog.Infow("processing miner", "current", i+1, "total", len(miners))

		// maybe recycle the head.
		once.Load().(*gosync.Once).Do(func() {
			var err error
			head, err = cl.ChainHead(context.Background())
			if err != nil {
				log.Fatalf("failed to fetch head: %s; aborting", err)
			}
			log.Infow("refreshed head", "head", head)
		})

		// fetch the miner's info.
		mi, err := cl.StateMinerInfo(context.Background(), m, head.Key())
		if err != nil {
			ch <- &Result{
				Timestamp: time.Now(),
				Miner:     m,
				Error:     fmt.Errorf("failed to get miner info: %s", err),
			}
			continue Outer
		}

		minerlog.Infow("fetched miner info", "peer_id", mi.PeerId)

		// convert multiaddrs.
		var addrs []multiaddr.Multiaddr
		for _, addr := range mi.Multiaddrs {
			a, err := multiaddr.NewMultiaddrBytes(addr)
			if err != nil {
				minerlog.Warnf("failed to interpret multiaddrs", err)
				ch <- &Result{
					Timestamp: time.Now(),
					Miner:     m,
					PeerID:    mi.PeerId,
					Error:     fmt.Errorf("failed to interpret multiaddr: %s", err),
				}
				continue Outer
			}
			addrs = append(addrs, a)
		}

		tasksWg.Add(1)
		go func(m address.Address) {
			defer tasksWg.Done()
			ch <- examineMiner(m, mi.PeerId, addrs, minerlog)
		}(m)
	}

	cancel()

	tasksWg.Wait()
	close(ch)

	wg.Wait()

	log.Infow("finished")
}

func connectBootstrappers(ch chan *Result) {
	addrs, err := addrutil.ParseAddresses(context.Background(), bootstrappers)
	if err != nil {
		log.Fatalf("failed to parse addresses: %s", err)
	}
	for _, ai := range addrs {
		ai := ai

		log.Infow("connecting to bootstrapper", "id", ai.ID)

		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = host.Connect(ctx, ai)
		cancel()

		took := time.Since(start)
		log.Infow("bootstrapper connection result", "id", ai.ID, "took", took, "ok", err == nil, "error", err)

		ch <- &Result{
			PeerID:       &ai.ID,
			Addrs:        ai.Addrs,
			Bootstrapper: true,
			Actions: []Action{
				{Kind: "direct", Success: err == nil, Error: err, Latency: took},
			},
		}
	}
}

func constructLibp2pHost() (core.Host, error) {
	opts := []libp2p.Option{
		libp2p.NoListenAddrs,
		libp2p.Ping(true),
		libp2p.UserAgent("filnetdiag"),
		libp2p.NATPortMap(),
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
	}

	return libp2p.New(context.Background(), opts...)
}

func constructDHT() (*dht.IpfsDHT, error) {
	dhtopts := []dht.Option{
		dht.Mode(dht.ModeClient),
		dht.Datastore(sync.MutexWrap(datastore.NewMapDatastore())),
		dht.Validator(record.NamespacedValidator{"pk": record.PublicKeyValidator{}}),
		dht.ProtocolPrefix("/fil/kad/testnetnet"),
		dht.QueryFilter(dht.PublicQueryFilter),
		dht.RoutingTableFilter(dht.PublicRoutingTableFilter),
		dht.DisableProviders(),
		dht.DisableValues(),
	}

	return dht.New(context.Background(), host, dhtopts...)
}

func examineMiner(miner address.Address, id *peer.ID, minerAddr []multiaddr.Multiaddr, minerlog *zap.SugaredLogger) *Result {
	minerlog.Infow("examining miner")
	defer minerlog.Infow("done with miner")

	result := &Result{
		Timestamp: time.Now(),
		Miner:     miner,
		PeerID:    id,
		Addrs:     minerAddr,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if id == nil {
		addrInfo, err := peer.AddrInfosFromP2pAddrs(minerAddr...)
		if err != nil {
			minerlog.Warnf("no miner peer ID; could not extract from multiaddrs", "error", err)
			msg := fmt.Sprintf("no miner peer ID; could not extract from multiaddrs: %s", err)
			result.Notes = append(result.Notes, msg)
			return result
		}
		if len(addrInfo) == 0 {
			minerlog.Warnf("no miner peer ID; zero multiaddrs")
			msg := fmt.Sprintf("no miner peer ID; zero multiaddrs")
			result.Notes = append(result.Notes, msg)
			return result
		}
		if len(addrInfo) > 1 {
			minerlog.Warnf("no miner peer ID; inconsistent peer IDs in multiaddrs", "addr_info", addrInfo)
			msg := fmt.Sprintf("[%s] no miner peer ID; inconsistent peer IDs in multiaddrs: %v", miner, addrInfo)
			result.Notes = append(result.Notes, msg)
			return result
		}
		id = &addrInfo[0].ID
	}

	minerlog = minerlog.With("peer_id", *id)

	connect := func(kind string, ai peer.AddrInfo) error {
		if host.Network().Connectedness(ai.ID) == network.Connected {
			minerlog.Infow("peer was connected")
			result.WasConnected = true
		}
		_ = host.Network().ClosePeer(ai.ID)
		host.Peerstore().ClearAddrs(ai.ID)

		start := time.Now()
		err := host.Connect(ctx, ai)
		took := time.Since(start)
		minerlog.Infow("connection result", "took", took, "error", err)

		result.Actions = append(result.Actions, Action{
			Kind:    kind,
			Success: err == nil,
			Error:   err,
			Latency: took,
		})
		return err
	}

	if err := connect("dial", peer.AddrInfo{ID: *id, Addrs: minerAddr}); err == nil {
		return result
	}

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	minerlog.Infow("trying DHT")

	result.TriedDHT = true
	start := time.Now()

	addrInfo, err := d.FindPeer(ctx, *id)
	took := time.Since(start)
	result.Actions = append(result.Actions, Action{
		Kind:    "dht_lookup",
		Success: err == nil,
		Error:   err,
		Latency: took,
	})
	minerlog.Infow("DHT lookup result", "took", took, "ok", err == nil, "error", err)
	if err != nil {
		return result
	}

	result.DHTAddrs = addrInfo.Addrs
	_ = connect("dht_dial", addrInfo)
	return result
}

func processResults(ch chan *Result) {
	defer wg.Done()

	var (
		now  = time.Now()
		name = fmt.Sprintf("filnetdiag.%s.out", now.Format(time.RFC3339))
	)

	file, err := os.Create(name)
	if err != nil {
		log.Fatalw("failed to create file; aborting", "file", name, "error", err)
	}
	defer file.Close()

	log.Infof("writing results to file: %s", name)

	enc := json.NewEncoder(file)
	for res := range ch {
		err := enc.Encode(res)
		if err != nil {
			log.Fatalw("failed to encode result; aborting", "error", err)
		}
	}
}
