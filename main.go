package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap/zapcore"

	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-record"

	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
)

const (
	// Endpoint is the endpoint to use for accessing mainnet state. Only used in
	// some checks.
	Endpoint  = "https://api.node.glif.io"
	DHTPrefix = "/fil/kad/testnetnet"
	UploadURL = "https://filreports.raul.io/upload"
)

var (
	// cl is a JSON-RPC client initialized to point to glif's node.
	cl client
	// host is a libp2p host.
	host core.Host
	// d is a libp2p DHT client.
	d *dht.IpfsDHT
	// log is the global logger.
	log *zap.SugaredLogger
)

// client is a minimal JSON-RPC client proxy with only the methods used by this
// tool.
type client struct {
	ChainHead        func(context.Context) (*types.TipSet, error)
	StateListMiners  func(context.Context, types.TipSetKey) ([]address.Address, error)
	StateMinerInfo   func(context.Context, address.Address, types.TipSetKey) (miner.MinerInfo, error)
	StateNetworkName func(context.Context) (dtypes.NetworkName, error)
	StateMinerPower  func(context.Context, address.Address, types.TipSetKey) (*api.MinerPower, error)
}

// Check represents a check that was performed by this diagnostics tool.
type Check struct {
	// Kind is the kind of check; some values are: "dial", "dht_lookup", "dht_dial".
	Kind string `json:",omitempty"`
	// Success is whether this check succeeded or not.
	Success bool `json:",omitempty"`
	// Error is an optional error message in case the check failed.
	Error string `json:",omitempty"`
	// Took is the amount of time this check took, in milliseconds.
	TookMs int64 `json:",omitempty"`
}

// ReportHeader is a report header. It must be output as the first line of
// every report.
type ReportHeader struct {
	// Header is always true, to signify that this is a header record.
	Header bool
	// Timestamp is the timestamp at which this diagnostics job was started.
	Timestamp time.Time
	// Kind is the kind of report.
	Kind string
	// From is the public-facing IP address of the machine where the report
	// was generated, as reported by various services.
	From map[string]net.IP
}

// ResultCommon is struct containing common fields for embedding inside
// result records.
type ResultCommon struct {
	// Kind is the kind of result record.
	Kind string
	// Timestamp is the time at which this result was emitted.
	Timestamp time.Time
	// Errors contains global errors associated with a result record.
	Errors []string `json:",omitempty"`
}

var mainFlags struct {
	upload    bool
	uploadUrl string
}

func main() {
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	cfg.DisableStacktrace = true
	l, _ := cfg.Build(zap.WithCaller(false))
	log = l.Sugar()
	defer log.Sync()

	// get all miners from glif.
	closer, err := jsonrpc.NewClient(context.Background(), Endpoint, "Filecoin", &cl, nil)
	if err != nil {
		log.Fatalf("failed to create JSON-RPC client: %s; aborting", err)
	}
	defer closer()

	// instantiate the app.
	app := &cli.App{
		Name:  "netdiag",
		Usage: "Filecoin networking diagnostics tool",
		Commands: []*cli.Command{
			genMinerCacheCmd,
			checkBootstrappersCmd,
			checkMinersCmd,
			checkBlockPublishersCmd,
			analyzeCmd,
			serverCmd,
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "upload",
				Usage:       "whether to upload results to reports server",
				Value:       false,
				Destination: &mainFlags.upload,
			},
			&cli.StringFlag{
				Name:        "upload-url",
				Usage:       "URL to upload the report to",
				Value:       UploadURL,
				Destination: &mainFlags.uploadUrl,
			},
		},
		Before: func(_ *cli.Context) error {
			// construct the host.
			host, err = constructLibp2pHost()
			if err != nil {
				return fmt.Errorf("failed to instantiate libp2p host: %w; aborting", err)
			}
			// construct the DHT.
			d, err = constructDHT()
			if err != nil {
				return fmt.Errorf("failed to construct DHT: %w; aborting", err)
			}
			return nil
		},
		After: func(_ *cli.Context) error {
			_ = host.Close()
			_ = d.Close()
			return nil
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
		libp2p.UserAgent("netdiag"),
		libp2p.NATPortMap(),
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
	}

	return libp2p.New(context.Background(), opts...)
}

func constructDHT() (*dht.IpfsDHT, error) {
	dhtopts := []dht.Option{
		dht.Mode(dht.ModeClient),
		dht.Datastore(dssync.MutexWrap(datastore.NewMapDatastore())),
		dht.Validator(record.NamespacedValidator{"pk": record.PublicKeyValidator{}}),
		dht.ProtocolPrefix(DHTPrefix),
		dht.QueryFilter(dht.PublicQueryFilter),
		dht.RoutingTableFilter(dht.PublicRoutingTableFilter),
		dht.DisableProviders(),
		dht.DisableValues(),
	}

	return dht.New(context.Background(), host, dhtopts...)
}

func writeReport(path string, ch chan interface{}) {
	file, err := os.Create(path)
	if err != nil {
		log.Fatalw("failed to create file; aborting", "file", path, "error", err)
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	for res := range ch {
		err := enc.Encode(res)
		if err != nil {
			log.Fatalw("failed to encode result; aborting", "error", err)
		}
	}
}

// dial performs a libp2p dial check.
func dial(ctx context.Context, ai peer.AddrInfo, kind string) Check {
	log := log.With("peer_id", ai.ID)

	if host.Network().Connectedness(ai.ID) == network.Connected {
		log.Infow("peer was connected")
	}
	_ = host.Network().ClosePeer(ai.ID)
	host.Peerstore().ClearAddrs(ai.ID)

	log.Infow("dialling")
	start := time.Now()
	err := host.Connect(ctx, ai)
	took := time.Since(start)
	log.Infow("dial result", "ok", err == nil, "took", took, "error", err)

	return Check{
		Kind:    kind,
		Success: err == nil,
		Error:   errorMsg(err),
		TookMs:  took.Milliseconds(),
	}
}

func errorMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func createHeader(kind string) *ReportHeader {
	h := &ReportHeader{
		Header:    true,
		Kind:      kind,
		Timestamp: time.Now(),
		From:      make(map[string]net.IP),
	}

	// services are the URLs of the services we're querying.
	services := []string{
		"https://myexternalip.com/raw",
		"https://api.ipify.org",
		"https://ip.seeip.org",
		"https://ipapi.co/ip/",
	}

	// ips will accumulate the responses in the same position as the URL of the
	// service we requested it from.
	ips := make([]net.IP, len(services))

	// fan out queries.
	var wg sync.WaitGroup
	for i, url := range services {
		url := url
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			log := log.With("service", url)

			log.Infow("querying for external-facing IP address")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				log.Warnw("failed to create HTTP request to public IP introspection service", "error", err)
				return
			}

			start := time.Now()
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Warnw("failed to request address from public IP introspection service", "error", err)
				return
			}
			bytes, err := ioutil.ReadAll(res.Body)
			if err != nil {
				log.Warnw("failed to parse response from public IP introspection service", "error", err)
				return
			}
			ip := net.ParseIP(string(bytes))

			log.Infow("obtained IP address", "ip", ip, "took", time.Since(start))

			ips[i] = ip
		}()
	}

	// collect the results.
	wg.Wait()
	for i, url := range services {
		h.From[url] = ips[i]
	}
	return h
}

func maybeUploadReport(filename string) {
	if !mainFlags.upload {
		log.Infof("upload disabled; skipping")
		return
	}
	if mainFlags.uploadUrl == "" {
		log.Infof("no upload URL; skipping")
		return
	}

	file, err := os.Open(filename)
	if err != nil {
		log.Warnw("could not open report for uploading", "filename", filename, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", mainFlags.uploadUrl, file)
	if err != nil {
		log.Warnw("failed to construct HTTP POST request for upload", "error", err)
		return
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warnw("failed upload report", "error", err)
		return
	}

	msg, _ := ioutil.ReadAll(res.Body)
	if status := res.StatusCode; status != http.StatusOK {
		log.Warnw("non-200 status received from server; report rejected", "status", status, "msg", string(msg))
		return
	}

	log.Infow("report uploaded successfully; please use this identifier to refer to it", "identifier", string(msg))
}

func stringMaddrs(addrs []multiaddr.Multiaddr) []string {
	ret := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ret = append(ret, a.String())
	}
	return ret
}
