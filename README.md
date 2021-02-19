# Filecoin Network Diagnosis tool

filnetdiag is a tool for performing network connectivity checks. It can perform
several checks:

* `check-bootstrappers`: verifies and records whether bootstrapper nodes are
  reachable.
* `check-miners`: verifies and records whether miners are connectable. For each
  miner, it attempts to resolve multiaddrs from the DHT and from their miner
  actor. It subsequently tries to connect to them by establishing a libp2p 
  connection.
* `check-block-publishers`: verifies and records whether nodes publishing blocks
  in blocks gossipsub topic are connectable. It subscribes to the topic, and for
  each message, it attempts to locate the author's peer ID in the DHT. If found,
  it attempts to establish a libp2p connection.

There are 60k+ miners registered in the Filecoin network, but around 1400 of 
them are actively mining (as of 19th Feburary 2021). To speed up the
`check-miners` check, this repo ships with a cache of all miners, in descending
order of their quality-adjusted power. By default, the `check-miners` check runs
against the 100 top miners, but you can adjust this passing in a different value
to the `--top` flag.

To regenerate the cache, use the `gen-miner-cache` subcommand.

Finally, there is a WIP `analyze` to process reports and print a summary.

## Author/maintainer

@raulk.

## License

Dual-licensed: [MIT](./LICENSE-MIT), [Apache Software License v2](./LICENSE-APACHE), by way of the
[Permissive License Stack](https://protocol.ai/blog/announcing-the-permissive-license-stack/).