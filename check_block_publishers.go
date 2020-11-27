package main

import (
	"context"
	"encoding/json"
	"fmt"
	gosync "sync"
	"time"

	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsub_pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/minio/blake2b-simd"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
)

var checkBlockPublishersFlags struct {
	duration time.Duration
}

var checkBlockPublishersCmd = &cli.Command{
	Name:        "check-block-publishers",
	Description: "run connectivity checks against block publishers",
	Action:      runCheckBlockPublishers,
	Flags: []cli.Flag{
		&cli.DurationFlag{
			Name:        "duration",
			Usage:       "how long to check for",
			Value:       10 * time.Minute,
			Destination: &checkBlockPublishersFlags.duration,
		},
	},
}

type BlockPublisherResult struct {
	ResultCommon
	// BlockCID is the CID of the received block.
	BlockCID cid.Cid `json:",omitempty"`
	// PeerID is the peer ID of the publisher.
	PeerID  peer.ID               `json:",omitempty"`
	Addrs   []multiaddr.Multiaddr `json:",omitempty"`
	Actions []Check               `json:",omitempty"`
}

type logEventTracer struct {
	logger *zap.SugaredLogger
}

var _ pubsub.EventTracer = (*logEventTracer)(nil)

func (l *logEventTracer) Trace(evt *pubsub_pb.TraceEvent) {
	m, _ := json.Marshal(evt)
	l.logger.Debug(string(m))
}

func runCheckBlockPublishers(_ *cli.Context) error {
	var (
		wg       gosync.WaitGroup
		ch       = make(chan interface{}, 16)
		filename = fmt.Sprintf("diag.blockpublishers.%s.out", time.Now().Format(time.RFC3339))
	)

	log.Infof("writing results to file: %s", filename)

	wg.Add(1)
	go func() {
		defer wg.Done()
		writeReport(filename, ch)
	}()

	ch <- createHeader("blockpublishers")

	// turn off the mesh in bootstrappers -- only do gossip and PX
	pubsub.GossipSubD = 0
	pubsub.GossipSubDscore = 0
	pubsub.GossipSubDlo = 0
	pubsub.GossipSubDhi = 0
	pubsub.GossipSubDout = 0
	pubsub.GossipSubDlazy = 64
	pubsub.GossipSubGossipFactor = 0.25
	pubsub.GossipSubPruneBackoff = 5 * time.Minute

	gs, err := pubsub.NewGossipSub(context.Background(), host,
		pubsub.WithPeerExchange(true),
		pubsub.WithMessageIdFn(func(m *pubsub_pb.Message) string {
			hash := blake2b.Sum256(m.Data)
			return string(hash[:])
		}),
		pubsub.WithEventTracer(&logEventTracer{logger: log.Named("pubsub")}),
	)
	if err != nil {
		return fmt.Errorf("failed to instantiate gossipsub: %w", err)
	}

	topic, err := gs.Join("/fil/blocks/testnetnet")
	if err != nil {
		return fmt.Errorf("failed to join blocks topic: %w", err)
	}

	log.Infow("connecting to bootstrappers", "count", len(bootstrappers))
	connectBootstrappers(ch)

	_ = d.Bootstrap(context.Background())

	sub, err := topic.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to blocks topic: %w", err)
	}
	defer sub.Cancel()

	log.Infof("subscribed to blocks topic; waiting for blocks")

	ctx, cancel := context.WithTimeout(context.Background(), checkBlockPublishersFlags.duration)
	defer cancel()

	checked := make(map[peer.ID]struct{})

	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break // context fired.
			}
			log.Warnf("subscription failed: %s", err)
			continue
		}

		id, err := peer.IDFromBytes(msg.From)
		if err != nil {
			log.Warnf("invalid peer ID: %s", err)
			continue
		}

		block, err := types.DecodeBlockMsg(msg.GetData())
		if err != nil {
			log.Warnf("unparseable block received")
			continue
		}

		log := log.With("peer_id", id)
		log.Infow("block received", "cid", block.Cid())

		if _, ok := checked[id]; ok {
			log.Infow("peer already checked; skipping")
			continue
		} else {
			checked[id] = struct{}{}
		}

		result := &BlockPublisherResult{
			ResultCommon: ResultCommon{Timestamp: time.Now()},
			BlockCID:     block.Cid(),
			PeerID:       id,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		action, ai, err := performDHTLookup(ctx, id, log)
		result.Actions = append(result.Actions, action)
		cancel()
		if err != nil {
			ch <- result
			continue
		}

		result.Addrs = ai.Addrs

		// dial the addrinfo returned by the DHT.
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		action = dial(ctx, ai, "dht_dial")
		result.Actions = append(result.Actions, action)
		ch <- result
		cancel()
	}

	close(ch)

	wg.Wait()

	return nil
}
