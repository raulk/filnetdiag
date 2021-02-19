package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/avast/retry-go"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/power"
	"github.com/urfave/cli/v2"
	"go.uber.org/ratelimit"
	"golang.org/x/sync/errgroup"
)

var genMinerCacheFlags struct {
	ratelimit uint
}

var genMinerCacheCmd = &cli.Command{
	Name:        "gen-miner-cache",
	Description: "generates a cache of all miners, in descending power order",
	Action:      runGenMinerCache,
	Flags: []cli.Flag{
		&cli.UintFlag{
			Name:        "ratelimit",
			Usage:       "number of miners to query per second",
			Value:       2,
			Destination: &genMinerCacheFlags.ratelimit,
		},
	},
}

func runGenMinerCache(cctx *cli.Context) error {
	head, err := cl.ChainHead(context.Background())
	if err != nil {
		return err
	}

	log.Infow("fetching miners from chain head", "head", head)
	unordered, err := cl.StateListMiners(context.Background(), head.Key())
	if err != nil {
		log.Fatalf("failed to list miners in network: %s; aborting", err)
	}
	log.Infow("obtained miners", "count", len(unordered))

	type minerPower struct {
		addr  address.Address
		power power.Claim
	}

	var (
		minerPowers = make([]minerPower, len(unordered)) // concurrent indexed access, not racy.
		limit       = ratelimit.New(int(genMinerCacheFlags.ratelimit), ratelimit.WithoutSlack)
	)

	errgrp, _ := errgroup.WithContext(cctx.Context)
	for i, m := range unordered {
		i := i
		m := m
		errgrp.Go(func() error {
			limit.Take()

			log.Infow("querying miner power", "miner", m, "current", i, "total", len(unordered))

			var pow *api.MinerPower
			err := retry.Do(func() (err error) {
				pow, err = cl.StateMinerPower(context.Background(), m, head.Key())
				return err
			}, retry.Delay(500*time.Millisecond), retry.Attempts(10), retry.OnRetry(func(n uint, err error) {
				log.Warnw("failed to obtain miner power", "miner", m, "attempt", n, "error", err)
			}))
			if err != nil {
				return fmt.Errorf("all attempts to obtain miner power exhausted; aborting")
			}

			minerPowers[i] = minerPower{m, pow.MinerPower}
			log.Infow("finished querying miner's power", "miner", m, "qadj_power", pow.MinerPower.QualityAdjPower)

			return nil
		})
	}

	if err := errgrp.Wait(); err != nil {
		return err
	}

	sort.Slice(minerPowers, func(i, j int) bool {
		// reverse sort == descending order.
		return minerPowers[j].power.QualityAdjPower.LessThan(minerPowers[i].power.QualityAdjPower)
	})

	log.Info("all miner powers obtained")

	file, err := os.Create("miners-cache.data")
	if err != nil {
		log.Fatalf("failed to create miners-cache.data file: %s", err)
	}

	defer file.Close()
	log.Infof("created file: %s", file.Name())

	w := bufio.NewWriter(file)
	defer w.Flush()
	for _, m := range minerPowers {
		_, _ = fmt.Fprintln(w, m.addr, m.power.QualityAdjPower)
	}

	log.Infof("all miners written in descending order of power in file: %s", file.Name())
	return nil
}
