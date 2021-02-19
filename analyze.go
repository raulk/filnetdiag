package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v2"
)

var analyzeFlags struct {
	ratelimit uint
}

var analyzeCmd = &cli.Command{
	Name:        "analyze",
	Description: "analyze a report",
	Action:      runAnalyze,
	ArgsUsage:   "<report_path>",
}

func runAnalyze(c *cli.Context) error {
	path := c.Args().First()
	if len(path) == 0 {
		return fmt.Errorf("expected path to report")
	}

	// open the file.
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("file does not exist")
	}
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	decoder := json.NewDecoder(file)

	// parse the report header.
	var header ReportHeader
	if err = decoder.Decode(&header); err != nil {
		return fmt.Errorf("failed to decode header: %w", err)
	}

	// dispatch to analyzer depending on report kind.
	switch k := header.Kind; k {
	case "bootstrappers", "blockpublishers":
		return fmt.Errorf("analyzer for report kind %s not implemented yet", k)
	case "miners":
		var results []*MinersResult
		for i := 2; ; i++ { // starting at line 2.
			var result MinersResult
			if err := decoder.Decode(&result); err == io.EOF {
				break
			} else if err != nil {
				log.Infof("skipping record on line: %d; %s", i, err)
				continue // skip this record.
			}
			results = append(results, &result)
		}
		return analyzeMinersReport(header, results)

	default:
		return fmt.Errorf("unrecognized report kind: %s", k)
	}

}

func analyzeMinersReport(header ReportHeader, results []*MinersResult) error {
	// TODO TODO TODO unfinished

	// inefficient, but we expect reports to be small. This could be turned into
	// a Go template with a backing struct.
	total := len(results)
	fmt.Printf("Report kind:\t%s\n", header.Kind)
	fmt.Printf("Miners analyzed:\t%d\n", total)
	fmt.Printf("Miners with peer ID:\t%d / %d\n", func() (have int) {
		for _, r := range results {
			if r.PeerID != nil {
				have++
			}
		}
		return have
	}(), total)
	fmt.Printf("Miners with no peer ID:\n")
	for _, r := range results {
		if r.PeerID == nil {
			fmt.Printf("\t%s\n", r.Miner)
		}
	}
	fmt.Printf("Miners with peer ID:\t%d / %d\n", func() (have int) {
		for _, r := range results {
			if len(r.MinerStateMaddrs) > 0 {
				have++
			}
		}
		return have
	}(), total)
	return nil
}
