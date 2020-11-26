package main

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	core "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/urfave/cli/v2"
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

var (
	cl   client
	host core.Host
	d    *dht.IpfsDHT
	log  *zap.SugaredLogger
)

type client struct {
	ChainHead        func(context.Context) (*types.TipSet, error)
	StateListMiners  func(context.Context, types.TipSetKey) ([]address.Address, error)
	StateMinerInfo   func(context.Context, address.Address, types.TipSetKey) (miner.MinerInfo, error)
	StateNetworkName func(context.Context) (dtypes.NetworkName, error)
	StateMinerPower  func(context.Context, address.Address, types.TipSetKey) (*api.MinerPower, error)
}

type Action struct {
	Kind    string `json:",omitempty"`
	Success bool   `json:",omitempty"`
	Error   string `json:",omitempty"`
	Latency int64  `json:",omitempty"`
}

type ResultCommon struct {
	Timestamp time.Time
	Kind      string
	Errors    []string `json:",omitempty"`
}

func main() {
	l, _ := zap.NewDevelopment(zap.WithCaller(false))
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

	app := &cli.App{
		Name:  "netdiag",
		Usage: "Filecoin networking diagnostics tool",
		Commands: []*cli.Command{
			genMinerCacheCmd,
			probeBootstrappersCmd,
			probeMinersCmd,
			probeBlockPublishersCmd,
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Warnf("%+v", err)
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

func writeResults(path string, ch chan interface{}) {
	file, err := os.Create(path)
	if err != nil {
		log.Fatalw("failed to create file; aborting", "file", path, "error", err)
	}
	defer file.Close()

	log.Infof("writing results to file: %s", path)

	enc := json.NewEncoder(file)
	for res := range ch {
		err := enc.Encode(res)
		if err != nil {
			log.Fatalw("failed to encode result; aborting", "error", err)
		}
	}
}

func connect(ctx context.Context, ai peer.AddrInfo) Action {
	if host.Network().Connectedness(ai.ID) == network.Connected {
		log.Infow("peer was connected")
	}
	_ = host.Network().ClosePeer(ai.ID)
	host.Peerstore().ClearAddrs(ai.ID)

	log := log.With("peer_id", ai.ID)

	log.Infow("connecting")
	start := time.Now()
	err := host.Connect(ctx, ai)
	took := time.Since(start)
	log.Infow("connection result", "ok", err == nil, "took", took, "error", err)

	return Action{
		Success: err == nil,
		Error:   errorMsg(err),
		Latency: took.Milliseconds(),
	}
}

func errorMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
